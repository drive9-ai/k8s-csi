package driver

import (
	"errors"
	"net"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSignalShutdownStopsServerWithoutTouchingMounts(t *testing.T) {
	stateDir := t.TempDir()
	store := newMountStateStore(stateDir, newHostRuntime())
	starting := validStartingState(t)
	active := validActiveState(t)
	if err := store.Write(starting); err != nil {
		t.Fatalf("write starting: %v", err)
	}
	if err := store.Write(active); err != nil {
		t.Fatalf("write active: %v", err)
	}
	before, err := store.Read(active.VolumeID)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}

	runtime := &fakeHostRuntime{}
	server := newFakeGRPCLifecycleServer()
	signals := make(chan os.Signal, 1)
	recoveryCancelled := make(chan struct{})
	var cancelOnce sync.Once
	cancelRecovery := func() {
		cancelOnce.Do(func() { close(recoveryCancelled) })
	}
	signals <- syscall.SIGTERM

	if err := runServerUntilSignal(server, nil, signals, cancelRecovery, time.Second); err != nil {
		t.Fatalf("runServerUntilSignal(): %v", err)
	}
	select {
	case <-recoveryCancelled:
	default:
		t.Fatal("recovery work was not cancelled")
	}
	if !server.gracefulStopped() {
		t.Fatal("gRPC server was not gracefully stopped")
	}
	if calls := runtime.Calls(); len(calls) != 0 {
		t.Fatalf("signal shutdown touched host runtime: %#v", calls)
	}
	after, err := store.Read(active.VolumeID)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("signal shutdown changed active state: before=%#v after=%#v", before, after)
	}
}

func TestRunShutdownReturnsServeFailureWithoutMountCleanup(t *testing.T) {
	server := newFakeGRPCLifecycleServer()
	server.serveErr = errors.New("serve failed")
	signals := make(chan os.Signal)
	cancelCalls := 0
	err := runServerUntilSignal(server, nil, signals, func() { cancelCalls++ }, time.Second)
	if !errors.Is(err, server.serveErr) {
		t.Fatalf("runServerUntilSignal() error = %v, want %v", err, server.serveErr)
	}
	if cancelCalls != 0 {
		t.Fatalf("serve failure unexpectedly ran signal cancellation %d times", cancelCalls)
	}
}

type fakeGRPCLifecycleServer struct {
	mu           sync.Mutex
	stop         chan struct{}
	stopOnce     sync.Once
	serveErr     error
	gracefulStop bool
}

func newFakeGRPCLifecycleServer() *fakeGRPCLifecycleServer {
	return &fakeGRPCLifecycleServer{stop: make(chan struct{})}
}

func (s *fakeGRPCLifecycleServer) Serve(net.Listener) error {
	if s.serveErr != nil {
		return s.serveErr
	}
	<-s.stop
	return nil
}

func (s *fakeGRPCLifecycleServer) GracefulStop() {
	s.mu.Lock()
	s.gracefulStop = true
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *fakeGRPCLifecycleServer) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
}

func (s *fakeGRPCLifecycleServer) gracefulStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gracefulStop
}
