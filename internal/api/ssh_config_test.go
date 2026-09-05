package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/waterlens/wmux/internal/config"
	"github.com/waterlens/wmux/internal/sshconfig"
	"github.com/waterlens/wmux/internal/store"
)

type fakeSSHConfigDiscoverer struct {
	mu sync.Mutex

	result       sshconfig.Result
	discoverErr  error
	resolveValue sshconfig.Candidate
	resolveErr   error
	resolveFunc  func(string) (sshconfig.Candidate, error)

	discoverCalls int
	resolveCalls  int
	resolvedAlias string
}

func (f *fakeSSHConfigDiscoverer) Discover(context.Context) (sshconfig.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discoverCalls++
	result := f.result
	result.Candidates = append([]sshconfig.Candidate(nil), result.Candidates...)
	return result, f.discoverErr
}

func (f *fakeSSHConfigDiscoverer) Resolve(_ context.Context, alias string) (sshconfig.Candidate, error) {
	f.mu.Lock()
	f.resolveCalls++
	f.resolvedAlias = alias
	resolve := f.resolveFunc
	value, err := f.resolveValue, f.resolveErr
	f.mu.Unlock()
	if resolve != nil {
		return resolve(alias)
	}
	return value, err
}

func (f *fakeSSHConfigDiscoverer) calls() (discover, resolve int, alias string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoverCalls, f.resolveCalls, f.resolvedAlias
}

func TestSSHConfigConfiguredPathDoesNotExposeIdentityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ssh-config")
	contents := []byte("Host lab\n  HostName lab.internal\n  User deploy\n  Port 2202\n  IdentityFile /secret/PRIVATE_KEY_NAME\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := newAPIFixture(t, apiOptions{config: config.Config{SSHConfigPath: path}})
	server, cookie := fixture.api, fixture.cookie
	response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("discover configured path: %d %s", response.Code, response.Body.String())
	}
	var body sshConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Source != path || len(body.Candidates) != 1 || !body.Candidates[0].HasIdentityFile {
		t.Fatalf("configured discovery = %#v", body)
	}
	if bytes.Contains(response.Body.Bytes(), []byte("PRIVATE_KEY_NAME")) || bytes.Contains(response.Body.Bytes(), []byte("/secret/")) {
		t.Fatalf("discovery leaked IdentityFile details: %s", response.Body.String())
	}
}

func TestSSHConfigRoutesRequireAuthAndSameOrigin(t *testing.T) {
	fixture := newAPIFixture(t, apiOptions{})
	server, cookie := fixture.api, fixture.cookie
	fake := &fakeSSHConfigDiscoverer{result: sshconfig.Result{Available: true, Source: "~/.ssh/config", Candidates: []sshconfig.Candidate{}}}
	server.sshConfig = fake

	if response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated discovery = %d %s", response.Code, response.Body.String())
	}
	if response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "lab"}, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated import = %d %s", response.Code, response.Body.String())
	}
	response := performJSONWithOrigin(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "lab"}, cookie, "https://attacker.example")
	if response.Code != http.StatusForbidden || responseErrorCode(t, response) != "invalid_origin" {
		t.Fatalf("cross-origin import = %d %s", response.Code, response.Body.String())
	}
	if discover, resolve, _ := fake.calls(); discover != 0 || resolve != 0 {
		t.Fatalf("rejected requests reached discoverer: Discover=%d Resolve=%d", discover, resolve)
	}
}

func TestDiscoverSSHConfigIsReadOnlyAndMarksExactExistingHost(t *testing.T) {
	fixture := newAPIFixture(t, apiOptions{})
	server, database, cookie := fixture.api, fixture.database, fixture.cookie
	existing, err := database.CreateHost(context.Background(), store.Host{
		Name: "Existing", Address: "lab.internal", Port: 2222, Username: "alice", AuthType: store.HostAuthAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSSHConfigDiscoverer{result: sshconfig.Result{
		Available: true,
		Source:    "~/.ssh/config",
		Candidates: []sshconfig.Candidate{
			{Alias: "lab", Address: "lab.internal", Port: 2222, Username: "alice", HasIdentityFile: true, Unsupported: []string{}},
			{Alias: "jumped", Address: "jump.internal", Port: 22, Username: "bob", Unsupported: []string{"ProxyJump"}},
			{Alias: "case-differs", Address: "LAB.INTERNAL", Port: 2222, Username: "alice", Unsupported: []string{}},
		},
	}}
	server.sshConfig = fake
	var probes atomic.Int32
	server.probeSSH = func(context.Context, string, string) (string, string, error) {
		probes.Add(1)
		return "", "", errors.New("probe must not run")
	}

	response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("discover: %d %s", response.Code, response.Body.String())
	}
	var body sshConfigResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Available || body.Source != "~/.ssh/config" || len(body.Candidates) != 3 {
		t.Fatalf("discovery response = %#v", body)
	}
	if candidate := body.Candidates[0]; candidate.ExistingHostID != existing.ID || !candidate.HasIdentityFile || candidate.Unsupported == nil {
		t.Fatalf("matched candidate = %#v", candidate)
	}
	if body.Candidates[1].ExistingHostID != "" || len(body.Candidates[1].Unsupported) != 1 {
		t.Fatalf("unsupported candidate = %#v", body.Candidates[1])
	}
	if body.Candidates[2].ExistingHostID != existing.ID {
		t.Fatalf("normalized address did not match existing host: %#v", body.Candidates[2])
	}
	hosts, err := database.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].ID != existing.ID {
		t.Fatalf("GET mutated hosts: %#v", hosts)
	}
	if discover, resolve, _ := fake.calls(); discover != 1 || resolve != 0 {
		t.Fatalf("discoverer calls = Discover %d, Resolve %d", discover, resolve)
	}
	if probes.Load() != 0 {
		t.Fatalf("GET performed %d SSH probe(s)", probes.Load())
	}
}

func TestConcurrentSSHConfigImportCreatesOneHost(t *testing.T) {
	fixture := newAPIFixture(t, apiOptions{})
	server, database, cookie := fixture.api, fixture.database, fixture.cookie
	server.sshConfig = &fakeSSHConfigDiscoverer{resolveValue: sshconfig.Candidate{
		Alias: "lab", Address: "lab.internal", Port: 22, Username: "alice", Unsupported: []string{},
	}}
	handler := server.Handler()
	const requests = 8
	codes := make(chan int, requests)
	var ready sync.WaitGroup
	ready.Add(requests)
	start := make(chan struct{})
	var finished sync.WaitGroup
	finished.Add(requests)
	for range requests {
		go func() {
			defer finished.Done()
			request := httptest.NewRequest(http.MethodPost, "/api/hosts/import-ssh-config", bytes.NewBufferString(`{"alias":"lab"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Cookie", cookie)
			recorder := httptest.NewRecorder()
			ready.Done()
			<-start
			handler.ServeHTTP(recorder, request)
			codes <- recorder.Code
		}()
	}
	ready.Wait()
	close(start)
	finished.Wait()
	close(codes)
	created, conflicts := 0, 0
	for code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("concurrent import status = %d", code)
		}
	}
	if created != 1 || conflicts != requests-1 {
		t.Fatalf("concurrent imports: created=%d conflicts=%d", created, conflicts)
	}
	hosts, err := database.ListHosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 {
		t.Fatalf("concurrent imports created %d hosts", len(hosts))
	}
}

func TestImportSSHConfigReResolvesWithoutCredentialsFingerprintOrProbe(t *testing.T) {
	fixture := newAPIFixture(t, apiOptions{})
	server, database, cookie := fixture.api, fixture.database, fixture.cookie
	fake := &fakeSSHConfigDiscoverer{
		result: sshconfig.Result{Available: true, Source: "~/.ssh/config", Candidates: []sshconfig.Candidate{
			{Alias: "lab", Address: "old.internal", Port: 22, Username: "old", Unsupported: []string{}},
		}},
		resolveValue: sshconfig.Candidate{
			Alias: "lab", Address: " new.internal ", Port: 2202, Username: " deploy ", HasIdentityFile: true, Unsupported: []string{},
		},
	}
	server.sshConfig = fake
	var probes atomic.Int32
	server.probeSSH = func(context.Context, string, string) (string, string, error) {
		probes.Add(1)
		return "SHA256:unexpected", "ssh-ed25519", nil
	}
	if response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie); response.Code != http.StatusOK {
		t.Fatalf("initial discovery: %d %s", response.Code, response.Body.String())
	}
	response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": " lab "}, cookie)
	if response.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", response.Code, response.Body.String())
	}
	var public hostResponse
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if public.Name != "lab" || public.Address != "new.internal" || public.Port != 2202 || public.Username != "deploy" || public.AuthType != store.HostAuthAgent {
		t.Fatalf("imported response = %#v", public)
	}
	if public.HasSecret || public.Fingerprint != "" {
		t.Fatalf("imported response has secret or fingerprint: %#v", public)
	}
	stored, err := database.GetHost(context.Background(), public.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.EncryptedCredentials) != 0 || stored.Fingerprint != "" || stored.AuthType != store.HostAuthAgent {
		t.Fatalf("import persisted credential/fingerprint: %#v", stored)
	}
	if discover, resolve, alias := fake.calls(); discover != 1 || resolve != 1 || alias != "lab" {
		t.Fatalf("discoverer calls = Discover %d, Resolve %d, alias %q", discover, resolve, alias)
	}
	if probes.Load() != 0 {
		t.Fatalf("import performed %d SSH probe(s)", probes.Load())
	}
}

func TestImportSSHConfigRejectsUnsupportedMissingDuplicateAndInvalid(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{resolveValue: sshconfig.Candidate{
			Alias: "jumped", Address: "internal", Port: 22, Username: "owner", Unsupported: []string{"ProxyJump"},
		}}
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "jumped"}, cookie)
		assertAPIError(t, response, http.StatusUnprocessableEntity, "ssh_config_unsupported")
	})

	t.Run("alias not found", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{resolveErr: errors.Join(errors.New("safe wrapper"), sshconfig.ErrAliasNotFound)}
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "missing"}, cookie)
		assertAPIError(t, response, http.StatusNotFound, "ssh_config_host_not_found")
	})

	t.Run("duplicate normalized endpoint", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, database, cookie := fixture.api, fixture.database, fixture.cookie
		if _, err := database.CreateHost(context.Background(), store.Host{
			Name: "Existing", Address: "lab.internal", Port: 22, Username: "alice", AuthType: store.HostAuthAgent,
		}); err != nil {
			t.Fatal(err)
		}
		server.sshConfig = &fakeSSHConfigDiscoverer{resolveValue: sshconfig.Candidate{
			Alias: "lab", Address: " LAB.INTERNAL ", Port: 22, Username: " alice ", Unsupported: []string{},
		}}
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "lab"}, cookie)
		assertAPIError(t, response, http.StatusConflict, "host_exists")
	})

	t.Run("invalid resolved candidate", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{resolveValue: sshconfig.Candidate{
			Alias: "bad", Address: "https://not-an-ssh-host", Port: 22, Username: "owner", Unsupported: []string{},
		}}
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "bad"}, cookie)
		assertAPIError(t, response, http.StatusUnprocessableEntity, "ssh_config_invalid")
	})

	t.Run("only alias is accepted", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		fake := &fakeSSHConfigDiscoverer{}
		server.sshConfig = fake
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]any{"alias": "lab", "address": "attacker"}, cookie)
		assertAPIError(t, response, http.StatusBadRequest, "invalid_request")
		if _, resolve, _ := fake.calls(); resolve != 0 {
			t.Fatal("invalid request reached resolver")
		}
	})
}

func TestSSHConfigMissingAndFailuresHaveStableSafeResponses(t *testing.T) {
	t.Run("public source", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{result: sshconfig.Result{
			Available: false, Source: "/Users/private-account/.ssh/config", Candidates: []sshconfig.Candidate{},
		}}
		response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
		var body sshConfigResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || body.Source != "~/.ssh/config" || body.Candidates == nil {
			t.Fatalf("default source response = %#v; status=%d", body, response.Code)
		}

		server.config.SSHConfigPath = "/mounted/config"
		response = performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || body.Source != "/mounted/config" {
			t.Fatalf("configured source response = %#v; status=%d", body, response.Code)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = sshconfig.New(filepath.Join(t.TempDir(), "missing-config"))
		response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
		if response.Code != http.StatusOK {
			t.Fatalf("missing config: %d %s", response.Code, response.Body.String())
		}
		var body sshConfigResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Available || body.Source == "" || body.Candidates == nil || len(body.Candidates) != 0 {
			t.Fatalf("missing config response = %#v; body=%s", body, response.Body.String())
		}
	})

	t.Run("invalid syntax", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{discoverErr: errors.New("parse /secret/config: token SUPER_SECRET")}
		response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
		assertAPIError(t, response, http.StatusUnprocessableEntity, "ssh_config_invalid")
		if bytes.Contains(response.Body.Bytes(), []byte("SUPER_SECRET")) || bytes.Contains(response.Body.Bytes(), []byte("/secret/config")) {
			t.Fatalf("invalid config response leaked details: %s", response.Body.String())
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{discoverErr: &fs.PathError{Op: "open", Path: "/secret/home/.ssh/config", Err: fs.ErrPermission}}
		response := performJSON(t, server.Handler(), http.MethodGet, "/api/hosts/ssh-config", nil, cookie)
		assertAPIError(t, response, http.StatusServiceUnavailable, "ssh_config_unavailable")
		if bytes.Contains(response.Body.Bytes(), []byte("/secret/home")) {
			t.Fatalf("unavailable config response leaked path: %s", response.Body.String())
		}
	})

	t.Run("resolve failure", func(t *testing.T) {
		fixture := newAPIFixture(t, apiOptions{})
		server, cookie := fixture.api, fixture.cookie
		server.sshConfig = &fakeSSHConfigDiscoverer{resolveErr: errors.New("parse failed with PRIVATE_VALUE")}
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/hosts/import-ssh-config", map[string]string{"alias": "lab"}, cookie)
		assertAPIError(t, response, http.StatusUnprocessableEntity, "ssh_config_invalid")
		if bytes.Contains(response.Body.Bytes(), []byte("PRIVATE_VALUE")) {
			t.Fatalf("resolve error leaked details: %s", response.Body.String())
		}
	})
}

func assertAPIError(t *testing.T, response interface {
	Result() *http.Response
}, status int, code string) {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	if result.StatusCode != status {
		t.Fatalf("status = %d, want %d", result.StatusCode, status)
	}
	var body errorBody
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code {
		t.Fatalf("error code = %q, want %q", body.Error.Code, code)
	}
}

func responseErrorCode(t *testing.T, response interface {
	Result() *http.Response
}) string {
	t.Helper()
	result := response.Result()
	defer result.Body.Close()
	var body errorBody
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Code
}
