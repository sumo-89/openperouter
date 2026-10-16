// SPDX-License-Identifier:Apache-2.0

package routerconfiguration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/ovsdbtest"
)

// TestSetupWithManager runs the controller as SetupWithManager wires it, on a
// real API server, and counts the passes. The unit tests of the handler, of
// the event filter and of the repair sources pass whether or not
// SetupWithManager uses them.
func TestSetupWithManager(t *testing.T) {
	if testing.Short() {
		t.Skip("needs an API server")
	}
	env := startWiringEnv(t)

	t.Run("a burst of events is coalesced into one pass for the node", func(t *testing.T) {
		env.waitForQuiet(t)
		start := env.router.passCount()

		// Hold the first pass, so that every other event of the burst arrives
		// while it runs, and must coalesce behind it.
		env.router.hold()
		env.create(t, l2VNIForWiring("burst-0", 100, nil))
		env.router.waitEntered(t)
		const burst = 20
		for i := 1; i < burst; i++ {
			env.create(t, l2VNIForWiring(fmt.Sprintf("burst-%d", i), int32(100+i), nil))
		}
		env.waitForCachedL2VNIs(t, burst)
		env.router.release()
		env.waitForQuiet(t)

		passes := env.router.passesSince(start)
		if len(passes) == 0 || len(passes) > 2 {
			t.Fatalf("expected the burst of %d events to run at most 2 passes, got %d: %v", burst, len(passes), passes)
		}
		expectNodeRequests(t, passes)
	})

	t.Run("an object that selects another node does not start a pass", func(t *testing.T) {
		env.waitForQuiet(t)
		start := env.router.passCount()

		env.create(t, l2VNIForWiring("elsewhere", 200, map[string]string{"kubernetes.io/hostname": "other-node"}))
		require.Never(t, func() bool { return len(env.router.passesSince(start)) > 0 }, time.Second,
			20*time.Millisecond, "expected no pass for an L2VNI that selects another node")

		env.create(t, l2VNIForWiring("here", 201, map[string]string{"kubernetes.io/hostname": testNodeName}))
		env.waitForPassesSince(t, start, 1)
	})

	// The repair sources feed the controller through a runnable of the manager
	// and a raw source of the builder; each subtest fails without either.
	t.Run("the periodic resync starts passes without any object event", func(t *testing.T) {
		resync := startWiringManager(t, func(r *PERouterReconciler) { r.ResyncPeriod = 200 * time.Millisecond })
		resync.waitForPassesSince(t, 0, 1)
		// Wait until the start-up passes, and those of the node status they
		// write, are over: the passes then come at the resync's pace.
		require.Eventually(t, func() bool {
			passes := resync.router.passesSince(0)
			if len(passes) < 4 {
				return false
			}
			for i := len(passes) - 3; i < len(passes); i++ {
				if passes[i].at.Sub(passes[i-1].at) < 150*time.Millisecond {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond, "the passes never settled to the resync's pace")

		start := resync.router.passCount()
		require.Eventually(t, func() bool { return len(resync.router.passesSince(start)) >= 5 }, 2*time.Second,
			20*time.Millisecond, "expected a pass about every 200ms with no object event, got %v",
			resync.router.passesSince(start))
		seen := resync.router.passCount()
		require.Eventually(t, func() bool { return resync.router.passCount() > seen }, 600*time.Millisecond,
			10*time.Millisecond, "expected the passes to keep coming")
		expectNodeRequests(t, resync.router.passesSince(start))
	})

	t.Run("an OVSDB change to an L2VNI host port starts a pass", func(t *testing.T) {
		const portName = hostnetwork.HostVethPrefix + hostnetwork.EvpnInfix + "100"
		saved := hostnetwork.OVSSocketPath
		hostnetwork.OVSSocketPath = "unix:" + ovsdbtest.Serve(t)
		t.Cleanup(func() { hostnetwork.OVSSocketPath = saved })
		writer := connectOVSWriter(t)
		createOVSHostPort(t, writer, portName)

		watched := startWiringManager(t, func(r *PERouterReconciler) { r.WatchOVSPorts = true })
		watched.waitForPassesSince(t, 0, 1)
		// Longer than waitForQuiet: the node status a start-up pass writes
		// can start one more pass after a while, which would pass for one the
		// port watch started.
		watched.waitForQuietFor(t, 5*time.Second)

		// Break the port until a pass runs: the watch connects in the
		// background, and a change made before it is up goes unseen.
		start := watched.router.passCount()
		poll := time.NewTicker(300 * time.Millisecond)
		defer poll.Stop()
		for deadline := time.Now().Add(10 * time.Second); len(watched.router.passesSince(start)) == 0; <-poll.C {
			if time.Now().After(deadline) {
				t.Fatal("expected a broken L2VNI host port to start a pass within 10s")
			}
			setOVSInterfaceError(t, writer, portName, "could not open network device "+portName)
			setOVSInterfaceError(t, writer, portName, "")
		}
		expectNodeRequests(t, watched.router.passesSince(start))
	})
}

type wiringEnv struct {
	client client.Client
	cache  client.Reader
	router *recordingRouterProvider
}

// startWiringEnv starts the controller and waits until the passes of its
// start-up are over.
func startWiringEnv(t *testing.T) *wiringEnv {
	t.Helper()
	env := startWiringManager(t, func(*PERouterReconciler) {})
	// The subtests count passes from here on, so the passes of the start-up,
	// and those caused by the node status they write, must be over.
	env.waitForPassesSince(t, 0, 1)
	env.waitForQuiet(t)
	return env
}

// startWiringManager starts a manager on a new API server with the reconciler
// wired by SetupWithManager, after configure has adjusted it.
func startWiringManager(t *testing.T, configure func(*PERouterReconciler)) *wiringEnv {
	t.Helper()
	ctrl.SetLogger(logr.Discard())

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		// Where make test installs the binaries, for a plain go test or an IDE
		// run; KUBEBUILDER_ASSETS takes precedence when set.
		BinaryAssetsDirectory: filepath.Join("..", "..", "..", "bin", "k8s",
			fmt.Sprintf("1.31.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest (run make test once, or set KUBEBUILDER_ASSETS): %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("failed to stop envtest: %v", err)
		}
	})

	scheme := testScheme(t)
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	env := &wiringEnv{client: cli, router: &recordingRouterProvider{}}
	env.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}})
	env.create(t, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: testNodeName, Labels: map[string]string{"kubernetes.io/hostname": testNodeName},
	}})

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "reload.sock")
	serveFRRReloader(t, socketPath)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// -count=N builds the controller once per run in one process.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}
	env.cache = mgr.GetCache()
	reconciler := &PERouterReconciler{
		Client:               mgr.GetClient(),
		Scheme:               scheme,
		MyNode:               testNodeName,
		MyNamespace:          testNamespace,
		Logger:               testLogger(),
		FRRConfigPath:        filepath.Join(dir, "frr.conf"),
		FRRReloadSocket:      socketPath,
		RouterProvider:       env.router,
		DatapathConfigurator: &noopDatapathConfigurator{},
	}
	configure(reconciler)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		env.router.release()
		cancel()
		if err := <-stopped; err != nil {
			t.Errorf("manager failed: %v", err)
		}
	})
	return env
}

func (e *wiringEnv) create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := e.client.Create(context.Background(), obj); err != nil {
		t.Fatalf("failed to create %T %s: %v", obj, obj.GetName(), err)
	}
}

// waitForQuiet waits until no pass has started for half a second, so that a
// subtest counts only the passes it caused.
func (e *wiringEnv) waitForQuiet(t *testing.T) {
	t.Helper()
	e.waitForQuietFor(t, 500*time.Millisecond)
}

// waitForQuietFor waits until no pass has started for the given time.
func (e *wiringEnv) waitForQuietFor(t *testing.T, quietFor time.Duration) {
	t.Helper()
	last, since := e.router.passCount(), time.Now()
	quiet := func() bool {
		if n := e.router.passCount(); n != last {
			last, since = n, time.Now()
			return false
		}
		return time.Since(since) >= quietFor
	}
	if !assert.Eventually(t, quiet, 20*time.Second, 20*time.Millisecond) {
		t.Fatalf("the controller never went quiet, passes: %v", e.router.passesSince(0))
	}
}

func (e *wiringEnv) waitForPassesSince(t *testing.T, start, n int) {
	t.Helper()
	enough := func() bool { return len(e.router.passesSince(start)) >= n }
	if !assert.Eventually(t, enough, 10*time.Second, 10*time.Millisecond) {
		t.Fatalf("expected at least %d passes within 10s, got %v", n, e.router.passesSince(start))
	}
}

func expectNodeRequests(t *testing.T, passes []pass) {
	t.Helper()
	want := ctrl.Request{NamespacedName: types.NamespacedName{Name: testNodeName}}.String()
	for _, p := range passes {
		if p.request != want {
			t.Fatalf("expected every pass to be for the node's request %q, got %v", want, passes)
		}
	}
}

// waitForCachedL2VNIs waits until the manager's cache, and so the event
// handlers, have held n L2VNIs for 200 ms, which gives the handlers time to
// queue the last event.
func (e *wiringEnv) waitForCachedL2VNIs(t *testing.T, n int) {
	t.Helper()
	var since time.Time
	require.Eventually(t, func() bool {
		var l2vnis v1alpha1.L2VNIList
		if err := e.cache.List(context.Background(), &l2vnis); err != nil || len(l2vnis.Items) < n {
			return false
		}
		if since.IsZero() {
			since = time.Now()
		}
		return time.Since(since) >= 200*time.Millisecond
	}, 10*time.Second, 10*time.Millisecond, "the cache never held %d L2VNIs", n)
}

type pass struct {
	request string
	at      time.Time
}

func (p pass) String() string { return fmt.Sprintf("%s@%s", p.request, p.at.Format("15:04:05.000")) }

// recordingRouterProvider records every pass that gets as far as asking for
// the router, and can hold passes.
type recordingRouterProvider struct {
	mu      sync.Mutex
	passes  []pass
	gate    chan struct{}
	entered chan struct{}
}

func (p *recordingRouterProvider) New(ctx context.Context) (Router, error) {
	request, _ := ctx.Value(requestKey("request")).(string)
	p.mu.Lock()
	p.passes = append(p.passes, pass{request: request, at: time.Now()})
	gate, entered := p.gate, p.entered
	p.mu.Unlock()

	if gate != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
	}
	return &mockRouter{}, nil
}

func (p *recordingRouterProvider) NodeIndex(_ context.Context) (int, error) {
	return 0, nil
}

func (p *recordingRouterProvider) hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gate = make(chan struct{})
	p.entered = make(chan struct{}, 1)
}

func (p *recordingRouterProvider) waitEntered(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	entered := p.entered
	p.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no pass started within 10s")
	}
}

func (p *recordingRouterProvider) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gate != nil {
		close(p.gate)
		p.gate = nil
	}
}

func (p *recordingRouterProvider) passCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.passes)
}

func (p *recordingRouterProvider) passesSince(start int) []pass {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pass(nil), p.passes[start:]...)
}

func l2VNIForWiring(name string, vni int32, nodeLabels map[string]string) *v1alpha1.L2VNI {
	l2vni := &v1alpha1.L2VNI{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       v1alpha1.L2VNISpec{VNI: vni},
	}
	if nodeLabels != nil {
		l2vni.Spec.NodeSelector = &metav1.LabelSelector{MatchLabels: nodeLabels}
	}
	return l2vni
}

// serveFRRReloader serves a reloader that accepts every configuration on
// socketPath.
func serveFRRReloader(t *testing.T, socketPath string) {
	t.Helper()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", socketPath, err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
}
