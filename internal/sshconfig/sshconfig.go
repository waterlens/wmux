// Package sshconfig discovers safe, importable hosts from OpenSSH user
// configuration without invoking ssh or reading any private key material.
package sshconfig

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrAliasNotFound is returned when Resolve is asked for a host that was not
// declared as a positive, literal Host alias.
var ErrAliasNotFound = errors.New("sshconfig: alias not found")

// Candidate is the non-secret subset of an SSH host that wmux can import.
// Unsupported contains canonical OpenSSH option names such as ProxyJump.
type Candidate struct {
	Alias           string
	Address         string
	Username        string
	Port            int
	HasIdentityFile bool
	Unsupported     []string
}

// Result describes the configured source and every importable literal alias.
type Result struct {
	Available  bool
	Source     string
	Candidates []Candidate
}

// Discoverer loads SSH configuration afresh for every call, so edits made by
// the user are visible without restarting wmux.
type Discoverer interface {
	Discover(context.Context) (Result, error)
	Resolve(context.Context, string) (Candidate, error)
}

type discoverer struct {
	path         string
	homeOverride string
}

// New creates a Discoverer. An empty path selects ~/.ssh/config for the system
// account running wmux; HOME does not override the account's home directory.
func New(path string) Discoverer {
	return &discoverer{path: path}
}

// newWithHome is intentionally private. It lets tests model the account home
// independently from the process environment without changing global state.
func newWithHome(path, home string) Discoverer {
	return &discoverer{path: path, homeOverride: home}
}

type systemAccount struct {
	username string
	uid      string
	homeDir  string
}

func (d *discoverer) Discover(ctx context.Context) (Result, error) {
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	account, err := runningAccount(d.homeOverride)
	if err != nil {
		return Result{}, err
	}
	source, err := sourcePath(d.path, account.homeDir)
	if err != nil {
		return Result{}, err
	}
	result := Result{Source: source, Candidates: []Candidate{}}
	document, available, err := loadDocument(ctx, source, account)
	if err != nil {
		return Result{}, err
	}
	result.Available = available
	if !available {
		return result, nil
	}
	aliases, err := document.literalAliases(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(aliases) == 0 {
		return result, nil
	}
	result.Candidates = make([]Candidate, 0, len(aliases))
	for _, alias := range aliases {
		if err := contextError(ctx); err != nil {
			return Result{}, err
		}
		candidate, err := document.resolve(ctx, alias, account)
		if err != nil {
			return Result{}, err
		}
		result.Candidates = append(result.Candidates, candidate)
	}
	return result, nil
}

func (d *discoverer) Resolve(ctx context.Context, alias string) (Candidate, error) {
	if err := contextError(ctx); err != nil {
		return Candidate{}, err
	}
	account, err := runningAccount(d.homeOverride)
	if err != nil {
		return Candidate{}, err
	}
	source, err := sourcePath(d.path, account.homeDir)
	if err != nil {
		return Candidate{}, err
	}
	document, available, err := loadDocument(ctx, source, account)
	if err != nil {
		return Candidate{}, err
	}
	found := false
	if available {
		found, err = document.hasLiteralAlias(ctx, alias)
		if err != nil {
			return Candidate{}, err
		}
	}
	if !available || !found {
		return Candidate{}, fmt.Errorf("%w: %s", ErrAliasNotFound, alias)
	}
	if err := contextError(ctx); err != nil {
		return Candidate{}, err
	}
	return document.resolve(ctx, alias, account)
}

func sourcePath(configured, accountHome string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		path = filepath.Join(accountHome, ".ssh", "config")
	} else {
		expanded, err := expandHome(path, accountHome)
		if err != nil {
			return "", err
		}
		path = expanded
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("sshconfig: resolve source path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func expandHome(path, accountHome string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path, nil
	}
	if strings.TrimSpace(accountHome) == "" {
		return "", errors.New("sshconfig: account home is empty")
	}
	if path == "~" {
		return accountHome, nil
	}
	return filepath.Join(accountHome, path[2:]), nil
}

func processUsername() (string, error) {
	account, err := runningAccount("")
	if err != nil {
		return "", err
	}
	return account.username, nil
}

func runningAccount(homeOverride string) (systemAccount, error) {
	current, err := user.Current()
	if err != nil {
		return systemAccount{}, fmt.Errorf("sshconfig: find process account: %w", err)
	}
	account := systemAccount{
		username: strings.TrimSpace(current.Username),
		uid:      strings.TrimSpace(current.Uid),
		homeDir:  strings.TrimSpace(current.HomeDir),
	}
	if account.username == "" {
		return systemAccount{}, errors.New("sshconfig: process account has an empty username")
	}
	if homeOverride != "" {
		account.homeDir = homeOverride
	}
	if account.homeDir == "" {
		return systemAccount{}, errors.New("sshconfig: process account has an empty home directory")
	}
	absoluteHome, err := filepath.Abs(account.homeDir)
	if err != nil {
		return systemAccount{}, fmt.Errorf("sshconfig: resolve process account home: %w", err)
	}
	account.homeDir = filepath.Clean(absoluteHome)
	return account, nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("sshconfig: nil context")
	}
	return ctx.Err()
}

type resolvedOptions struct {
	hostName     string
	hostNameSet  bool
	hostSource   string
	hostLine     int
	user         string
	userSet      bool
	userSource   string
	userLine     int
	port         string
	portSet      bool
	portSource   string
	portLine     int
	identityFile bool
	proxyJump    string
	jumpSet      bool
	proxyCommand string
	commandSet   bool
}

func (d document) resolve(ctx context.Context, alias string, account systemAccount) (Candidate, error) {
	options := resolvedOptions{}
	stack := map[string]bool{d.root.canonical: true}
	if err := d.apply(ctx, d.root, alias, &options, stack, 0); err != nil {
		return Candidate{}, err
	}

	candidate := Candidate{
		Alias:    alias,
		Address:  alias,
		Username: account.username,
		Port:     22,
	}
	if options.hostNameSet && options.hostName != "" {
		hostName, err := expandHostName(options.hostName, alias)
		if err != nil {
			return Candidate{}, fmt.Errorf("sshconfig: %s:%d: HostName: %w", options.hostSource, options.hostLine, err)
		}
		candidate.Address = hostName
	}
	if options.portSet {
		port, err := strconv.Atoi(options.port)
		if err != nil || port < 1 || port > 65535 {
			return Candidate{}, fmt.Errorf("sshconfig: %s:%d: invalid Port %q for alias %q", options.portSource, options.portLine, options.port, alias)
		}
		candidate.Port = port
	}
	if options.userSet && options.user != "" {
		username, err := expandUser(options.user, alias, candidate.Address, candidate.Port, account, options.proxyJump)
		if err != nil {
			return Candidate{}, fmt.Errorf("sshconfig: %s:%d: User: %w", options.userSource, options.userLine, err)
		}
		candidate.Username = username
	}
	candidate.HasIdentityFile = options.identityFile
	if options.jumpSet && !isNone(options.proxyJump) {
		candidate.Unsupported = append(candidate.Unsupported, "ProxyJump")
	}
	if options.commandSet && !isNone(options.proxyCommand) {
		candidate.Unsupported = append(candidate.Unsupported, "ProxyCommand")
	}
	if candidate.Unsupported == nil {
		candidate.Unsupported = []string{}
	}
	return candidate, nil
}

func (d document) apply(ctx context.Context, file configFile, alias string, options *resolvedOptions, stack map[string]bool, depth int) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("sshconfig: include depth exceeds %d at %s", maxIncludeDepth, file.path)
	}
	active := true
	for index, directive := range file.directives {
		if index%64 == 0 {
			if err := contextError(ctx); err != nil {
				return err
			}
		}
		switch directive.keyword {
		case "host":
			active = hostPatternsMatch(directive.arguments, alias)
			continue
		case "match":
			// Match all has no side effects and is target-independent. Every other
			// Match criterion is fail-closed; in particular, exec is never run.
			active = isMatchAll(directive.arguments)
			continue
		case "include":
			if !active {
				continue
			}
			if depth >= maxIncludeDepth {
				return fmt.Errorf("sshconfig: include depth exceeds %d at %s:%d", maxIncludeDepth, directive.source, directive.line)
			}
			included, err := d.includedFiles(ctx, directive)
			if err != nil {
				return err
			}
			for _, child := range included {
				if stack[child.canonical] {
					return fmt.Errorf("sshconfig: recursive Include cycle at %s", child.path)
				}
				stack[child.canonical] = true
				err := d.apply(ctx, child, alias, options, stack, depth+1)
				delete(stack, child.canonical)
				if err != nil {
					return err
				}
			}
			continue
		}
		if !active || len(directive.arguments) == 0 {
			continue
		}
		value := directive.arguments[0]
		switch directive.keyword {
		case "hostname":
			if !options.hostNameSet {
				options.hostName, options.hostNameSet = value, true
				options.hostSource, options.hostLine = directive.source, directive.line
			}
		case "user":
			if !options.userSet {
				options.user, options.userSet = value, true
				options.userSource, options.userLine = directive.source, directive.line
			}
		case "port":
			if !options.portSet {
				options.port, options.portSet = value, true
				options.portSource, options.portLine = directive.source, directive.line
			}
		case "identityfile":
			// IdentityFile is the OpenSSH exception to first-value-wins:
			// multiple matching declarations accumulate. We expose only
			// whether at least one real path was configured.
			if !isNone(value) {
				options.identityFile = true
			}
		case "proxyjump":
			if !options.jumpSet && !options.commandSet {
				options.proxyJump = strings.Join(directive.arguments, " ")
				options.jumpSet = true
			}
		case "proxycommand":
			if !options.commandSet && (!options.jumpSet || isNone(options.proxyJump)) {
				options.proxyCommand = strings.Join(directive.arguments, " ")
				options.commandSet = true
			}
		}
	}
	return nil
}

func isNone(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "none") || strings.TrimSpace(value) == ""
}

func expandHostName(value, alias string) (string, error) {
	return expandPercentTokens(value, func(token byte) (string, error) {
		switch token {
		case 'h':
			return alias, nil
		default:
			return "", fmt.Errorf("unsupported token %%%c", token)
		}
	})
}

func expandUser(value, alias, address string, port int, account systemAccount, proxyJump string) (string, error) {
	// Expanding arbitrary process environment variables into a value returned by
	// the discovery API would expose service secrets. Include retains environment
	// expansion because it is needed to locate config, but User is fail-closed.
	if strings.Contains(value, "${") {
		return "", errors.New("environment expansion is disabled for User")
	}
	return expandPercentTokens(value, func(token byte) (string, error) {
		switch token {
		case 'd':
			return account.homeDir, nil
		case 'h':
			return address, nil
		case 'i':
			if account.uid == "" {
				return "", errors.New("resolve %i user ID: empty user ID")
			}
			return account.uid, nil
		case 'j':
			if isNone(proxyJump) {
				return "", nil
			}
			return proxyJump, nil
		case 'l', 'L':
			hostname, err := os.Hostname()
			if err != nil {
				return "", fmt.Errorf("resolve %%%c local hostname: %w", token, err)
			}
			if token == 'L' {
				hostname, _, _ = strings.Cut(hostname, ".")
			}
			return hostname, nil
		case 'n':
			return alias, nil
		case 'p':
			return strconv.Itoa(port), nil
		case 'u':
			return account.username, nil
		case 'k':
			return "", errors.New("token %k requires HostKeyAlias, which discovery does not import")
		default:
			return "", fmt.Errorf("unsupported token %%%c", token)
		}
	})
}
