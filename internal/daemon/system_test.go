package daemon

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/killedoxygen/berth/internal/api"
	"github.com/killedoxygen/berth/internal/config"
	"github.com/killedoxygen/berth/internal/job"
	"github.com/killedoxygen/berth/internal/probe"
	"github.com/killedoxygen/berth/internal/units"
)

const alice, bob, root = 1001, 1002, 0

// sharedDaemon is a system-mode daemon that is never started: requests are
// fed to it directly as if from different users.
func sharedDaemon(t *testing.T, edit ...func(*config.Config)) *Daemon {
	t.Helper()
	cfg := config.Default(probe.Host{MemTotal: 64 * units.GiB, CPUs: 16, HomeDiskTotal: 500 * units.GiB})
	cfg.Backend.Kind = "process"
	for _, e := range edit {
		e(&cfg)
	}
	dir := t.TempDir()
	d, err := New(Options{Config: cfg, Paths: config.Paths{System: true, State: filepath.Join(dir, "state"),
		Socket: filepath.Join(dir, "sock")}, Self: "/bin/false", Version: "test", Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func pair(t *testing.T) (*api.Conn, *api.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *api.Conn {
		f := os.NewFile(uintptr(fd), "")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		return api.Wrap(c.(*net.UnixConn))
	}
	return conn(fds[0]), conn(fds[1])
}

func submitAs(t *testing.T, d *Daemon, uid int, spec job.Spec) *api.Response {
	t.Helper()
	if spec.Argv == nil {
		spec.Argv = []string{"/bin/true"}
	}
	if spec.Cwd == "" {
		spec.Cwd = "/"
	}
	spec.Disk = -1
	server, client := pair(t)
	defer client.Close()
	go func() {
		defer server.Close()
		d.submit(server, client_(uid), api.Request{Op: "submit", Spec: &spec}, nil)
	}()
	var resp api.Response
	if err := client.Recv(&resp); err != nil {
		t.Fatal(err)
	}
	return &resp
}

func client_(uid int) client { return client{pid: os.Getpid(), uid: uid} }

func ask(d *Daemon, uid int, req api.Request) *api.Response { return d.simple(client_(uid), req) }

func mustSubmit(t *testing.T, d *Daemon, uid int, spec job.Spec) *job.Job {
	t.Helper()
	resp := submitAs(t, d, uid, spec)
	if !resp.OK {
		t.Fatalf("submit as %d: %s", uid, resp.Error)
	}
	return resp.Job
}

func TestUsersCannotReachEachOthersJobs(t *testing.T) {
	d := sharedDaemon(t)
	secret := mustSubmit(t, d, alice, job.Spec{Argv: []string{"deploy", "--token=hunter2"}, Name: "alice-secret", Key: "k"})
	for _, op := range []string{"get", "cancel", "logs"} {
		if resp := ask(d, bob, api.Request{Op: op, ID: secret.ID}); resp.Error == "" || resp.Code != api.ENotFound {
			t.Fatalf("bob %s on alice's job: %+v", op, resp)
		}
	}
	prio := 9
	if resp := ask(d, bob, api.Request{Op: "update", ID: secret.ID, Update: &api.Update{Priority: &prio}}); resp.Code != api.ENotFound {
		t.Fatalf("bob updated alice's job: %+v", resp)
	}
	st := ask(d, bob, api.Request{Op: "status"}).Status
	for _, j := range st.Jobs {
		blob := strings.Join(j.Spec.Argv, " ") + j.Spec.Name + j.Spec.Cwd + j.Note
		if strings.Contains(blob, "hunter2") || strings.Contains(blob, "alice-secret") {
			t.Fatalf("status leaks alice's job to bob: %+v", j)
		}
	}
	if resp := ask(d, root, api.Request{Op: "get", ID: secret.ID}); resp.Job == nil || resp.Job.Spec.Name != "alice-secret" {
		t.Fatal("root sees every job")
	}
	if resp := ask(d, alice, api.Request{Op: "cancel", ID: secret.ID}); resp.Error != "" || resp.Job.State != job.Cancelled {
		t.Fatalf("alice cancels her own job: %+v", resp)
	}
}

func TestDependenciesOnlyOnOwnJobs(t *testing.T) {
	d := sharedDaemon(t)
	theirs := mustSubmit(t, d, alice, job.Spec{})
	if resp := submitAs(t, d, bob, job.Spec{After: []string{theirs.ID}}); resp.OK || !strings.Contains(resp.Error, "dependency") {
		t.Fatalf("bob depended on alice's job: %+v", resp)
	}
	mine := mustSubmit(t, d, bob, job.Spec{})
	mustSubmit(t, d, bob, job.Spec{After: []string{mine.ID[:6]}})
}

func TestGroupsArePerUserUnlessGlobal(t *testing.T) {
	d := sharedDaemon(t)
	a := mustSubmit(t, d, alice, job.Spec{Group: "repo"})
	b := mustSubmit(t, d, bob, job.Spec{Group: "repo"})
	if a.Spec.Group == b.Spec.Group {
		t.Fatalf("users share a lock: %q", a.Spec.Group)
	}
	g1 := mustSubmit(t, d, alice, job.Spec{Group: "global:gpu0"})
	g2 := mustSubmit(t, d, bob, job.Spec{Group: "global:gpu0"})
	if g1.Spec.Group != "global:gpu0" || g2.Spec.Group != g1.Spec.Group {
		t.Fatal("global groups are shared")
	}
}

func TestServicesAreForRootUnlessAllowed(t *testing.T) {
	d := sharedDaemon(t)
	if resp := submitAs(t, d, alice, job.Spec{Kind: job.Service}); resp.OK || !strings.Contains(resp.Error, "allow_services") {
		t.Fatalf("a user submitted a service: %+v", resp)
	}
	mustSubmit(t, d, root, job.Spec{Kind: job.Service})
	open := sharedDaemon(t, func(c *config.Config) { c.Users.AllowServices = true })
	mustSubmit(t, open, alice, job.Spec{Kind: job.Service})
}

func TestPerUserJobLimit(t *testing.T) {
	d := sharedDaemon(t, func(c *config.Config) { c.Users.MaxJobs = 2 })
	mustSubmit(t, d, alice, job.Spec{})
	mustSubmit(t, d, alice, job.Spec{})
	if resp := submitAs(t, d, alice, job.Spec{}); resp.OK || resp.Code != api.EDenied {
		t.Fatalf("third job accepted: %+v", resp)
	}
	mustSubmit(t, d, bob, job.Spec{})
	mustSubmit(t, d, root, job.Spec{})
}

func TestClaimChecksTheCaller(t *testing.T) {
	d := sharedDaemon(t)
	j := mustSubmit(t, d, alice, job.Spec{})
	d.mu.Lock()
	token := d.st.Get(j.ID).Token
	d.mu.Unlock()
	server, client := pair(t)
	go func() {
		defer server.Close()
		d.claim(server, client_(bob), api.Request{Op: "claim", ID: j.ID, Token: token})
	}()
	var resp api.Response
	client.Recv(&resp)
	client.Close()
	if resp.OK {
		t.Fatal("another user claimed a job's descriptors even with the token")
	}
}

func TestLogBudgetEvictsTheHeaviestUserFirst(t *testing.T) {
	mk := func(uid int, age int) *job.Job {
		return &job.Job{ID: fmt.Sprint(uid, "-", age), UID: uid, State: job.Succeeded, Finished: time.Unix(int64(1000+age), 0)}
	}
	a1, a2, a3, b1 := mk(alice, 1), mk(alice, 2), mk(alice, 3), mk(bob, 0)
	finished := []*job.Job{b1, a1, a2, a3} // oldest first: bob's is the oldest of all
	sizes := map[*job.Job]int64{a1: 10, a2: 10, a3: 10, b1: 8}
	got := evictions(finished, sizes, 15)
	if len(got) != 2 || got[0] != a1 || got[1] != a2 {
		t.Fatalf("want alice's two oldest logs (she uses 30 of 38), got %v", got)
	}
	got = evictions(finished, sizes, 30)
	for _, j := range got {
		if j == b1 && len(got) < 4 {
			t.Fatal("bob's log went before alice's usage dropped below his")
		}
	}
}
