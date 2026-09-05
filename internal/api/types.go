package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/waterlens/wmux/internal/store"
)

// maxDisplayNameLen bounds every name the browser renders for the user.
const maxDisplayNameLen = 80

// One message per resource, shared by creation and renaming so the same rule
// never reaches the user in two wordings.
var (
	errSessionName = fmt.Errorf("会话名称不能为空且不能超过 %d 个字符", maxDisplayNameLen)
	errHostName    = fmt.Errorf("主机名称不能为空且不能超过 %d 个字符", maxDisplayNameLen)
)

// validateDisplayName applies the length rule sessions and hosts share.
// Session creation is the one caller that accepts an empty name, because it
// then generates a default of its own.
func validateDisplayName(name string, allowEmpty bool, invalid error) error {
	if (name == "" && !allowEmpty) || len(name) > maxDisplayNameLen {
		return invalid
	}
	return nil
}

type setupInput struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginInput = setupInput

type hostInput struct {
	Name       string  `json:"name"`
	Address    string  `json:"address"`
	Port       int     `json:"port"`
	Username   string  `json:"username"`
	AuthType   string  `json:"authType"`
	Password   *string `json:"password,omitempty"`
	PrivateKey *string `json:"privateKey,omitempty"`
	Passphrase *string `json:"passphrase,omitempty"`
}

type hostResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Port        int       `json:"port"`
	Username    string    `json:"username"`
	AuthType    string    `json:"authType"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	HasSecret   bool      `json:"hasSecret"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type sshConfigImportInput struct {
	Alias string `json:"alias"`
}

type sshConfigCandidateResponse struct {
	Alias           string   `json:"alias"`
	Address         string   `json:"address"`
	Port            int      `json:"port"`
	Username        string   `json:"username"`
	HasIdentityFile bool     `json:"hasIdentityFile"`
	Unsupported     []string `json:"unsupported"`
	ExistingHostID  string   `json:"existingHostId,omitempty"`
}

type sshConfigResponse struct {
	Available  bool                         `json:"available"`
	Source     string                       `json:"source"`
	Candidates []sshConfigCandidateResponse `json:"candidates"`
}

type sessionResponse struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	HostID      *string `json:"hostId,omitempty"`
	HostName    *string `json:"hostName,omitempty"`
	Cwd         string  `json:"cwd,omitempty"`
	Command     string  `json:"command,omitempty"`
	Persistence string  `json:"persistence"`
	// Backend is the resolved persistence kind, not a multiplexer session name.
	Backend        string     `json:"backend,omitempty"`
	Status         string     `json:"status"`
	Generation     int        `json:"generation"`
	Cols           int        `json:"cols"`
	Rows           int        `json:"rows"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	LastAttachedAt *time.Time `json:"lastAttachedAt,omitempty"`
	Error          *string    `json:"error,omitempty"`
}

type sessionInput struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	HostID      string `json:"hostId,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Command     string `json:"command,omitempty"`
	Persistence string `json:"persistence"`
}

// sessionPatch carries product metadata only.
type sessionPatch struct {
	Name *string `json:"name,omitempty"`
}

func (v *setupInput) normalize() error {
	v.Username = strings.TrimSpace(v.Username)
	if len(v.Username) < 1 || len(v.Username) > 64 {
		return errors.New("用户名长度需要在 1 到 64 个字符之间")
	}
	if len(v.Password) < 10 {
		return errors.New("密码至少需要 10 个字符")
	}
	if len(v.Password) > 1024 {
		return errors.New("密码过长")
	}
	return nil
}

func (v *hostInput) normalize() error {
	v.Name = strings.TrimSpace(v.Name)
	v.Address = strings.TrimSpace(v.Address)
	v.Username = strings.TrimSpace(v.Username)
	v.AuthType = strings.TrimSpace(v.AuthType)
	if err := validateDisplayName(v.Name, false, errHostName); err != nil {
		return err
	}
	if v.Address == "" || len(v.Address) > 255 || strings.Contains(v.Address, "://") {
		return errors.New("请输入有效的主机名或 IP 地址")
	}
	if v.Port == 0 {
		v.Port = 22
	}
	if v.Port < 1 || v.Port > 65535 {
		return errors.New("SSH 端口必须在 1 到 65535 之间")
	}
	if v.Username == "" || len(v.Username) > 128 {
		return errors.New("SSH 用户名不能为空")
	}
	switch v.AuthType {
	case store.HostAuthPassword:
		if v.Password == nil || *v.Password == "" {
			return errors.New("密码认证需要填写密码")
		}
	case store.HostAuthKey:
		if v.PrivateKey == nil || strings.TrimSpace(*v.PrivateKey) == "" {
			return errors.New("私钥认证需要填写私钥")
		}
	case store.HostAuthAgent:
	default:
		return errors.New("不支持的 SSH 认证方式")
	}
	return nil
}

// inheritCredentials keeps the persisted secret for every credential an edit
// did not supply. The three fields use different rules on purpose: the host
// editor submits without retyping a password or a private key, so a blank one
// means "unchanged", while a blank passphrase is a real value because that is
// how a key with no passphrase is saved.
func (v *hostInput) inheritCredentials(stored store.Credentials) {
	if v.Password == nil || *v.Password == "" {
		v.Password = &stored.Password
	}
	if v.PrivateKey == nil || strings.TrimSpace(*v.PrivateKey) == "" {
		v.PrivateKey = &stored.PrivateKey
	}
	if v.Passphrase == nil {
		v.Passphrase = &stored.Passphrase
	}
}

func (v *sessionInput) normalize() error {
	v.Name = strings.TrimSpace(v.Name)
	v.Kind = strings.TrimSpace(v.Kind)
	v.HostID = strings.TrimSpace(v.HostID)
	v.Cwd = strings.TrimSpace(v.Cwd)
	v.Command = strings.TrimSpace(v.Command)
	v.Persistence = strings.TrimSpace(v.Persistence)
	// Creation may leave the name empty; createSession then generates one.
	if err := validateDisplayName(v.Name, true, errSessionName); err != nil {
		return err
	}
	if v.Kind != store.SessionKindLocal && v.Kind != store.SessionKindSSH {
		return errors.New("会话类型必须是 local 或 ssh")
	}
	if v.Kind == store.SessionKindSSH && v.HostID == "" {
		return errors.New("SSH 会话必须选择主机")
	}
	if v.Kind == store.SessionKindLocal {
		v.HostID = ""
	}
	if len(v.Cwd) > 4096 || len(v.Command) > 8192 {
		return errors.New("工作目录或启动命令过长")
	}
	if v.Persistence == "" {
		v.Persistence = store.SessionPersistenceAuto
	}
	switch v.Persistence {
	case store.SessionPersistenceAuto, store.SessionPersistenceTmux, store.SessionPersistenceScreen, store.SessionPersistenceNone:
	default:
		return errors.New("不支持的持久化模式")
	}
	return nil
}
