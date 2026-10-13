package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
)

func TestBindDaemonHealthPortFallsBackWithoutReleasingListener(t *testing.T) {
	preferred := healthPortForProfile("desktop-api.multica.ai")
	var attempts []string
	ln, port, err := bindDaemonHealthPort(preferred, func(network, addr string) (net.Listener, error) {
		attempts = append(attempts, addr)
		if addr == fmt.Sprintf("127.0.0.1:%d", preferred) {
			return nil, syscall.EACCES
		}
		return net.Listen(network, addr)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if len(attempts) != 2 || attempts[1] != "127.0.0.1:0" || port == preferred || port != ln.Addr().(*net.TCPAddr).Port {
		t.Fatalf("attempts=%v port=%d listener=%v", attempts, port, ln.Addr())
	}
	if second, err := net.Listen("tcp", ln.Addr().String()); err == nil {
		second.Close()
		t.Fatal("fallback listener was released before the caller could use it")
	}
}

func TestResolveHealthPortValidatesRecordAndFallsBack(t *testing.T) {
	mkProfiles(t, "desktop-api.multica.ai")
	profile := "desktop-api.multica.ai"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"running","profile":"desktop-api.multica.ai","pid":12345}`)
	}))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	if port == healthPortForProfile(profile) {
		t.Skip("test server selected the legacy profile port")
	}
	if err := publishDaemonPort(profile, daemonPortRecord{Port: port, PID: 12345}); err != nil {
		t.Fatal(err)
	}
	if got := resolveHealthPort(profile); got != port {
		t.Fatalf("resolved port=%d, want live record %d", got, port)
	}
	if err := publishDaemonPort(profile, daemonPortRecord{Port: port, PID: 999}); err != nil {
		t.Fatal(err)
	}
	if got := resolveHealthPort(profile); got != healthPortForProfile(profile) {
		t.Fatalf("resolved stale PID port=%d, want legacy %d", got, healthPortForProfile(profile))
	}
	removeDaemonPortIfOwned(profile, 12345)
	if _, err := os.Stat(daemonPortPathForProfile(profile)); err != nil {
		t.Fatalf("different PID removed the record: %v", err)
	}
	removeDaemonPortIfOwned(profile, 999)
	if _, err := os.Stat(daemonPortPathForProfile(profile)); !os.IsNotExist(err) {
		t.Fatalf("owner cleanup left record: %v", err)
	}
}

func TestResolveHealthPortRejectsAnotherProfile(t *testing.T) {
	mkProfiles(t, "desktop-api.multica.ai")
	profile := "desktop-api.multica.ai"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"running","profile":"other","pid":12345}`)
	}))
	defer server.Close()
	if err := publishDaemonPort(profile, daemonPortRecord{
		Port: server.Listener.Addr().(*net.TCPAddr).Port,
		PID:  12345,
	}); err != nil {
		t.Fatal(err)
	}
	if got := resolveHealthPort(profile); got != healthPortForProfile(profile) {
		t.Fatalf("resolved foreign daemon port=%d, want legacy %d", got, healthPortForProfile(profile))
	}
}

func TestDaemonProfileLockExcludesSecondProcessHandle(t *testing.T) {
	mkProfiles(t, "desktop-api.multica.ai")
	lock, err := acquireDaemonProfileLock("desktop-api.multica.ai", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	second, acquired, err := tryLockDaemonProfile(lock.Name())
	if err != nil {
		t.Fatal(err)
	}
	if second != nil {
		second.Close()
	}
	if acquired {
		t.Fatal("second handle acquired the active profile lock")
	}
}
