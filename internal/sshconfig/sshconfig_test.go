package sshconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestDiscoverMissingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "config")
	discoverer := New(path)
	result, err := discoverer.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Available {
		t.Fatal("missing config reported itself available")
	}
	if result.Source != absolute {
		t.Fatalf("source = %q, want %q", result.Source, absolute)
	}
	if result.Candidates == nil || len(result.Candidates) != 0 {
		t.Fatalf("missing config candidates = %#v, want an empty list", result.Candidates)
	}
	if _, err := discoverer.Resolve(context.Background(), "missing"); !errors.Is(err, ErrAliasNotFound) {
		t.Fatalf("Resolve missing alias error = %v, want ErrAliasNotFound", err)
	}
}

func TestDefaultPathUsesAccountHomeNotEnvironment(t *testing.T) {
	accountHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(accountHome, ".ssh", "config")
	writeConfig(t, path, "Host home-alias\n")

	result, err := newWithHome("", accountHome).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.Source != path {
		t.Fatalf("default source result = %#v, want available source %q", result, path)
	}
	if aliases := candidateAliases(result.Candidates); !reflect.DeepEqual(aliases, []string{"home-alias"}) {
		t.Fatalf("aliases = %#v", aliases)
	}
}

func TestRunningAccountAndPercentDIgnoreEnvironmentHome(t *testing.T) {
	current, err := user.Current()
	if err != nil || strings.TrimSpace(current.HomeDir) == "" {
		t.Skipf("system account home unavailable: %v", err)
	}
	t.Setenv("HOME", t.TempDir())

	account, err := runningAccount("")
	if err != nil {
		t.Fatal(err)
	}
	wantHome, err := filepath.Abs(current.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	if account.homeDir != filepath.Clean(wantHome) {
		t.Fatalf("account home = %q, want %q", account.homeDir, filepath.Clean(wantHome))
	}
	expanded, err := expandIncludeTokens("%d/fragments/*.conf", account)
	if err != nil {
		t.Fatal(err)
	}
	if expanded != filepath.Join(filepath.Clean(wantHome), "fragments", "*.conf") {
		t.Fatalf("%%d expansion = %q", expanded)
	}
}

func TestDiscoverRecursivelyIncludesGlobsInStableOrder(t *testing.T) {
	accountHome := t.TempDir()
	sshDir := filepath.Join(accountHome, ".ssh")
	root := filepath.Join(t.TempDir(), "custom-config")
	writeConfig(t, root, `
Include "conf.d/*.conf"
Host root root *.root !negated
  HostName root.example
`)
	writeConfig(t, filepath.Join(sshDir, "conf.d", "20-second.conf"), `
Host beta alpha
  HostName beta.example
`)
	writeConfig(t, filepath.Join(sshDir, "conf.d", "10-first.conf"), `
Include "more configs/nested.conf"
Host alpha *.wild !negative
  HostName alpha.example
`)
	writeConfig(t, filepath.Join(sshDir, "more configs", "nested.conf"), `
Host "quoted alias"
  HostName quoted.example
`)

	discoverer := newWithHome(root, accountHome)
	result, err := discoverer.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Available {
		t.Fatal("existing root config reported unavailable")
	}
	want := []string{"quoted alias", "alpha", "beta", "root"}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate order = %#v, want %#v", got, want)
	}
	for _, forbidden := range []string{"*.wild", "*.root", "!negative", "!negated"} {
		if slices.Contains(candidateAliases(result.Candidates), forbidden) {
			t.Fatalf("non-literal Host pattern %q became a candidate", forbidden)
		}
	}
	quoted, err := discoverer.Resolve(context.Background(), "quoted alias")
	if err != nil {
		t.Fatal(err)
	}
	if quoted.Address != "quoted.example" {
		t.Fatalf("quoted include candidate = %#v", quoted)
	}
	alpha, err := discoverer.Resolve(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Address != "alpha.example" {
		t.Fatalf("glob order did not preserve first value: %#v", alpha)
	}
}

func TestRelativeIncludeUsesAccountSSHDirectoryForCustomConfig(t *testing.T) {
	accountHome := t.TempDir()
	customDir := t.TempDir()
	root := filepath.Join(customDir, "custom.conf")
	writeConfig(t, root, "Include fragments/host.conf\n")
	writeConfig(t, filepath.Join(customDir, "fragments", "host.conf"), "Host wrong-base\n")
	writeConfig(t, filepath.Join(accountHome, ".ssh", "fragments", "host.conf"), "Host account-base\n")
	t.Setenv("HOME", t.TempDir())

	result, err := newWithHome(root, accountHome).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, []string{"account-base"}) {
		t.Fatalf("relative Include candidates = %#v", got)
	}
}

func TestResolveHonorsFirstValueWildcardInheritanceAndNegation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `
Host * !target
  HostName excluded.example
  User excluded
Host target
  HostName first.example
  HostName second.example
  User first-user
  User second-user
  Port 2201
  Port 2202
  IdentityFile "/private/key path"
  IdentityFile none
  ProxyJump bastion
  ProxyJump none
  ProxyCommand none
  ProxyCommand "nc %h %p"
Host *
  User fallback-user
  Port 2299
`)

	candidate, err := New(path).Resolve(context.Background(), "target")
	if err != nil {
		t.Fatal(err)
	}
	want := Candidate{
		Alias: "target", Address: "first.example", Username: "first-user", Port: 2201,
		HasIdentityFile: true, Unsupported: []string{"ProxyJump"},
	}
	if !reflect.DeepEqual(candidate, want) {
		t.Fatalf("candidate = %#v, want %#v", candidate, want)
	}
}

func TestResolveInheritsLaterWildcardDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `
Host app
  HostName app.internal
Host *
  User deploy
  Port 2222
  IdentityFile ~/.ssh/id_deploy
`)

	candidate, err := New(path).Resolve(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Address != "app.internal" || candidate.Username != "deploy" || candidate.Port != 2222 || !candidate.HasIdentityFile {
		t.Fatalf("wildcard-inherited candidate = %#v", candidate)
	}
}

func TestIdentityFileAccumulatesLikeOpenSSH(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `
Host app
  IdentityFile none
Host *
  IdentityFile ~/.ssh/id_later
`)

	candidate, err := New(path).Resolve(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if !candidate.HasIdentityFile {
		t.Fatalf("later matching IdentityFile was not accumulated: %#v", candidate)
	}
}

func TestResolveDefaultsAndRejectsWildcardOnlyNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `
Host plain
Host *.internal !blocked
  User wildcard
`)

	candidate, err := New(path).Resolve(context.Background(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	current, currentErr := user.Current()
	if currentErr != nil || current.Username == "" {
		if fallback := strings.TrimSpace(os.Getenv("USER")); fallback != "" {
			current = &user.User{Username: fallback}
		} else {
			t.Skip("running process username is unavailable")
		}
	}
	want := Candidate{Alias: "plain", Address: "plain", Username: current.Username, Port: 22, Unsupported: []string{}}
	if !reflect.DeepEqual(candidate, want) {
		t.Fatalf("default candidate = %#v, want %#v", candidate, want)
	}
	for _, alias := range []string{"host.internal", "blocked"} {
		if _, err := New(path).Resolve(context.Background(), alias); !errors.Is(err, ErrAliasNotFound) {
			t.Fatalf("Resolve(%q) error = %v, want ErrAliasNotFound", alias, err)
		}
	}
}

func TestQuotedAndEqualsSyntax(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `
Host="quoted"
  HostName = "box.example"
  User "name with spaces"
  Port="2022"
  IdentityFile "~/.ssh/key with spaces"
  ProxyCommand = "ssh -W %h:%p jump # literal"
`)

	candidate, err := New(path).Resolve(context.Background(), "quoted")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Address != "box.example" || candidate.Username != "name with spaces" || candidate.Port != 2022 || !candidate.HasIdentityFile {
		t.Fatalf("quoted candidate = %#v", candidate)
	}
	if !reflect.DeepEqual(candidate.Unsupported, []string{"ProxyCommand"}) {
		t.Fatalf("unsupported options = %#v", candidate.Unsupported)
	}
}

func TestUnsupportedProxyOptionsAreCanonicalAndNoneIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `
Host jump-first
  proxyjump bastion
  PROXYCOMMAND nc %h %p
Host command-first
  ProxyCommand nc %h %p
  ProxyJump bastion
Host jump-none-first
  ProxyJump none
  ProxyCommand nc %h %p
Host command-none-first
  ProxyCommand NONE
  ProxyJump bastion
`)

	result, err := New(path).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"jump-first":         {"ProxyJump"},
		"command-first":      {"ProxyCommand"},
		"jump-none-first":    {"ProxyCommand"},
		"command-none-first": {},
	}
	for _, candidate := range result.Candidates {
		if !reflect.DeepEqual(candidate.Unsupported, want[candidate.Alias]) {
			t.Fatalf("%s unsupported = %#v, want %#v", candidate.Alias, candidate.Unsupported, want[candidate.Alias])
		}
	}
}

func TestConditionalIncludeIsLazyAndDoesNotExposeAliases(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "config")
	included := filepath.Join(dir, "selected.conf")
	writeConfig(t, root, `
Host selected
  Include `+included+`
Host other
  HostName other.example
`)
	writeConfig(t, included, `
User selected-user
Host hidden-in-conditional-include
  HostName hidden.example
`)

	discoverer := New(root)
	result, err := discoverer.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, []string{"selected", "other"}) {
		t.Fatalf("conditional Include exposed aliases: %#v", got)
	}
	selected, err := discoverer.Resolve(context.Background(), "selected")
	if err != nil {
		t.Fatal(err)
	}
	if selected.Username != "selected-user" {
		t.Fatalf("active conditional Include was not applied: %#v", selected)
	}
	other, err := discoverer.Resolve(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	current, err := processUsername()
	if err != nil {
		t.Fatal(err)
	}
	if other.Username != current || other.Address != "other.example" {
		t.Fatalf("inactive conditional Include affected other host: %#v", other)
	}
	if _, err := discoverer.Resolve(context.Background(), "hidden-in-conditional-include"); !errors.Is(err, ErrAliasNotFound) {
		t.Fatalf("conditional alias error = %v, want ErrAliasNotFound", err)
	}
}

func TestIncludedHostStateDoesNotEscapeAndBareOptionsInheritActiveCall(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "config")
	stateChild := filepath.Join(dir, "state-child.conf")
	bareChild := filepath.Join(dir, "bare-child.conf")
	writeConfig(t, root, `
Host target
  Include `+stateChild+`
  Port 2200
Host bare
  Include `+bareChild+`
  Port 2300
`)
	writeConfig(t, stateChild, `
Host other
  User hidden
`)
	writeConfig(t, bareChild, `
User inherited-active
Host never
  User never
`)

	discoverer := New(root)
	target, err := discoverer.Resolve(context.Background(), "target")
	if err != nil {
		t.Fatal(err)
	}
	current, err := processUsername()
	if err != nil {
		t.Fatal(err)
	}
	if target.Port != 2200 || target.Username != current {
		t.Fatalf("included Host state escaped into caller: %#v", target)
	}
	bare, err := discoverer.Resolve(context.Background(), "bare")
	if err != nil {
		t.Fatal(err)
	}
	if bare.Port != 2300 || bare.Username != "inherited-active" {
		t.Fatalf("bare included option did not inherit active call: %#v", bare)
	}
}

func TestUniversalHostIncludeIsDiscoverableButNegatedHostIsFailClosed(t *testing.T) {
	dir := t.TempDir()
	universalRoot := filepath.Join(dir, "universal.conf")
	universalChild := filepath.Join(dir, "universal-child.conf")
	writeConfig(t, universalRoot, `
Host *
  Include `+universalChild+`
`)
	writeConfig(t, universalChild, `
Host from-universal-host
  HostName universal.example
`)

	result, err := New(universalRoot).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, []string{"from-universal-host"}) {
		t.Fatalf("Host * Include candidates = %#v", got)
	}
	if result.Candidates[0].Address != "universal.example" {
		t.Fatalf("Host * Include candidate = %#v", result.Candidates[0])
	}

	negatedRoot := filepath.Join(dir, "negated.conf")
	negatedChild := filepath.Join(dir, "negated-child.conf")
	writeConfig(t, negatedRoot, `
Host * !excluded
  Include `+negatedChild+`
Host excluded
  HostName excluded.example
`)
	writeConfig(t, negatedChild, "Host hidden-by-negated-condition\n")
	result, err = New(negatedRoot).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, []string{"excluded"}) {
		t.Fatalf("negated Host Include exposed candidates: %#v", got)
	}
}

func TestInactiveConditionalIncludeIsNeverOpened(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "config")
	nonRegular := filepath.Join(dir, "not-a-config-file")
	if err := os.Mkdir(nonRegular, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, root, `
Host selected
  Include `+nonRegular+`
Host other
  HostName other.example
`)

	candidate, err := New(root).Resolve(context.Background(), "other")
	if err != nil {
		t.Fatalf("inactive Include was opened: %v", err)
	}
	if candidate.Address != "other.example" {
		t.Fatalf("candidate = %#v", candidate)
	}
	if _, err := New(root).Resolve(context.Background(), "selected"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("active non-regular Include error = %v", err)
	}
}

func TestIdentityFileIsBooleanOnlyAndNeverRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	secretPath := filepath.Join(dir, "DO_NOT_LEAK_private_key_material")
	secretContents := "SUPER_SECRET_PRIVATE_KEY_CONTENT"
	// A nonexistent path proves discovery neither stats nor opens IdentityFile.
	writeConfig(t, path, "Host safe\n  IdentityFile \""+secretPath+"\"\n")

	result, err := New(path).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || !result.Candidates[0].HasIdentityFile {
		t.Fatalf("identity result = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{secretPath, filepath.Base(secretPath), secretContents} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("discovery result leaked %q: %s", leaked, encoded)
		}
	}
}

func TestDuplicateIncludesAndAliasesAreDeterministic(t *testing.T) {
	accountHome := t.TempDir()
	root := filepath.Join(t.TempDir(), "config")
	included := filepath.Join(accountHome, ".ssh", "shared.conf")
	writeConfig(t, root, "Include shared.conf\nInclude shared.conf\nHost after shared\n")
	writeConfig(t, included, "Host shared shared\n  User first\n")

	result, err := newWithHome(root, accountHome).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"shared", "after"}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("aliases = %#v, want %#v", got, want)
	}
}

func TestIncludeExpandsEnvironmentHomeAndLiteralPercent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	environmentDir := filepath.Join(home, "environment includes")
	t.Setenv("WMUX_SSH_CONFIG_FRAGMENTS", environmentDir)
	root := filepath.Join(home, ".ssh", "config")
	writeConfig(t, root, `
Include "${WMUX_SSH_CONFIG_FRAGMENTS}/*.conf"
Include "%d/token includes/literal%%.conf"
`)
	writeConfig(t, filepath.Join(environmentDir, "10-environment.conf"), "Host from-environment\n")
	writeConfig(t, filepath.Join(home, "token includes", "literal%.conf"), "Host from-tokens\n")

	result, err := newWithHome(root, home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"from-environment", "from-tokens"}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded Include aliases = %#v, want %#v", got, want)
	}
}

func TestIncludeRejectsMissingEnvironmentAndTargetDependentTokens(t *testing.T) {
	dir := t.TempDir()
	missingEnvironment := filepath.Join(dir, "missing-environment.conf")
	writeConfig(t, missingEnvironment, "Include ${WMUX_SSH_CONFIG_DEFINITELY_MISSING}/hosts\n")
	t.Setenv("WMUX_SSH_CONFIG_DEFINITELY_MISSING", "temporary-test-value")
	os.Unsetenv("WMUX_SSH_CONFIG_DEFINITELY_MISSING")
	if _, err := New(missingEnvironment).Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "WMUX_SSH_CONFIG_DEFINITELY_MISSING") || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("missing environment error = %v", err)
	}

	for _, token := range []string{"%h", "%n", "%r", "%p"} {
		path := filepath.Join(dir, "dependent-"+strings.TrimPrefix(token, "%")+".conf")
		writeConfig(t, path, "Include "+token+"/hosts.conf\n")
		if _, err := New(path).Discover(context.Background()); err == nil || !strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "target host") {
			t.Fatalf("target-dependent Include %s error = %v", token, err)
		}
	}
}

func TestHostNameExpandsAliasAndLiteralPercent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, "Host production\n  HostName node-%h-%%.example\n")

	candidate, err := New(path).Resolve(context.Background(), "production")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Address != "node-production-%.example" {
		t.Fatalf("expanded HostName = %q", candidate.Address)
	}

	writeConfig(t, path, "Host unsafe\n  HostName %n.example\n")
	if _, err := New(path).Resolve(context.Background(), "unsafe"); err == nil || !strings.Contains(err.Error(), "unsupported token %n") {
		t.Fatalf("unsupported HostName token error = %v", err)
	}
}

func TestUserRejectsPercentTokensButKeepsLiteralPercent(t *testing.T) {
	accountHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	for _, token := range []string{"%d", "%u", "%n", "%h", "%p", "%i", "%j", "%l", "%L", "%k"} {
		path := filepath.Join(t.TempDir(), "config")
		writeConfig(t, path, "Host production\n  User deploy-"+token+"\n")
		_, err := newWithHome(path, accountHome).Resolve(context.Background(), "production")
		if err == nil || !strings.Contains(err.Error(), "token "+token+" in User is not supported") {
			t.Fatalf("User %s error = %v, want an explicit rejection", token, err)
		}
	}

	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, "Host production\n  User deploy-100%%\n")
	candidate, err := newWithHome(path, accountHome).Resolve(context.Background(), "production")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Username != "deploy-100%" {
		t.Fatalf("literal percent User = %q", candidate.Username)
	}
}

func TestUserEnvironmentExpansionFailsClosedWithoutLeakingValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	secret := "SUPER_SECRET_SERVICE_TOKEN_DO_NOT_LEAK"
	t.Setenv("WMUX_SSHCONFIG_SECRET", secret)
	writeConfig(t, path, "Host unsafe-user\n  User ${WMUX_SSHCONFIG_SECRET}\n")

	_, err := New(path).Resolve(context.Background(), "unsafe-user")
	if err == nil || !strings.Contains(err.Error(), "environment expansion is disabled for User") {
		t.Fatalf("User environment error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("User environment error leaked the value: %v", err)
	}
	result, discoverErr := New(path).Discover(context.Background())
	if discoverErr == nil {
		t.Fatalf("Discover unexpectedly returned %#v", result)
	}
	encoded := fmt.Sprintf("%#v %v", result, discoverErr)
	if strings.Contains(encoded, secret) {
		t.Fatalf("Discover leaked the environment value: %s", encoded)
	}
}

func TestLiteralAliasesAndHostPatternsAreCaseSensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, `Host WorkBox
  HostName upper.example
  User upper
Host workbox
  HostName lower.example
  User lower
Host *.EXAMPLE
  User uppercase-pattern
Host app.example
  HostName app.internal
Host *
  User fallback
`)

	discoverer := New(path)
	result, err := discoverer.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, []string{"WorkBox", "workbox", "app.example"}) {
		t.Fatalf("aliases = %#v", got)
	}
	upper, err := discoverer.Resolve(context.Background(), "WorkBox")
	if err != nil {
		t.Fatal(err)
	}
	lower, err := discoverer.Resolve(context.Background(), "workbox")
	if err != nil {
		t.Fatal(err)
	}
	app, err := discoverer.Resolve(context.Background(), "app.example")
	if err != nil {
		t.Fatal(err)
	}
	if upper.Address != "upper.example" || upper.Username != "upper" {
		t.Fatalf("upper candidate = %#v", upper)
	}
	if lower.Address != "lower.example" || lower.Username != "lower" {
		t.Fatalf("lower candidate = %#v", lower)
	}
	if app.Username != "fallback" {
		t.Fatalf("case-mismatched wildcard applied: %#v", app)
	}
}

func TestIncludeCycleAndDepthAreBounded(t *testing.T) {
	accountHome := t.TempDir()
	sshDir := filepath.Join(accountHome, ".ssh")
	a := filepath.Join(sshDir, "a.conf")
	b := filepath.Join(sshDir, "b.conf")
	writeConfig(t, a, "Include b.conf\n")
	writeConfig(t, b, "Include a.conf\n")
	if _, err := newWithHome(a, accountHome).Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("include cycle error = %v", err)
	}

	for index := 0; index <= maxIncludeDepth+1; index++ {
		path := filepath.Join(sshDir, "depth", configName(index))
		if index == maxIncludeDepth+1 {
			writeConfig(t, path, "Host deepest\n")
		} else {
			writeConfig(t, path, "Include depth/"+configName(index+1)+"\n")
		}
	}
	first := filepath.Join(sshDir, "depth", configName(0))
	if _, err := newWithHome(first, accountHome).Discover(context.Background()); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("include depth error = %v", err)
	}
}

func TestMatchAllIsSupportedAndItsIncludeIsDiscoverable(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "config")
	included := filepath.Join(dir, "match-all.conf")
	writeConfig(t, root, `
Host matched
  HostName matched.example
Match all
  User match-all-user
  Include `+included+`
`)
	writeConfig(t, included, `
Host included-from-match-all
  HostName included.example
`)

	result, err := New(root).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateAliases(result.Candidates); !reflect.DeepEqual(got, []string{"matched", "included-from-match-all"}) {
		t.Fatalf("Match all candidates = %#v", got)
	}
	for _, candidate := range result.Candidates {
		if candidate.Username != "match-all-user" {
			t.Fatalf("Match all was not applied to %#v", candidate)
		}
	}
}

func TestMatchExecIsNeverExecuted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	sentinel := filepath.Join(dir, "must-not-exist")
	nonRegular := filepath.Join(dir, "must-not-open")
	if err := os.Mkdir(nonRegular, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "Match exec \"touch "+sentinel+"\"\n  Include "+nonRegular+"\n  HostName unsafe.example\nHost safe\n  HostName safe.example\n")

	candidate, err := New(path).Resolve(context.Background(), "safe")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Address != "safe.example" {
		t.Fatalf("candidate = %#v", candidate)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Match exec side effect exists or cannot be checked: %v", err)
	}
}

func TestCanceledContextStopsDiscoverAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, "Host canceled\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	discoverer := New(path)
	if _, err := discoverer.Discover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Discover canceled error = %v", err)
	}
	if _, err := discoverer.Resolve(ctx, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve canceled error = %v", err)
	}
}

func TestNonRegularConfigIsRejectedBeforeOpen(t *testing.T) {
	for _, path := range []string{t.TempDir(), os.DevNull} {
		_, err := New(path).Discover(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("Discover(%q) error = %v", path, err)
		}
	}
}

func TestInvalidPortIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeConfig(t, path, "Host bad-port\n  Port 70000\n")
	if _, err := New(path).Resolve(context.Background(), "bad-port"); err == nil || !strings.Contains(err.Error(), "invalid Port") {
		t.Fatalf("invalid port error = %v", err)
	}
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func candidateAliases(candidates []Candidate) []string {
	aliases := make([]string, len(candidates))
	for index, candidate := range candidates {
		aliases[index] = candidate.Alias
	}
	return aliases
}

func configName(index int) string {
	return "config-" + strconv.Itoa(index)
}
