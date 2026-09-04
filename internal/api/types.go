package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

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

type hostPatch struct {
	Name       *string `json:"name,omitempty"`
	Address    *string `json:"address,omitempty"`
	Port       *int    `json:"port,omitempty"`
	Username   *string `json:"username,omitempty"`
	AuthType   *string `json:"authType,omitempty"`
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

type sessionInput struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	HostID      string `json:"hostId,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	Command     string `json:"command,omitempty"`
	Persistence string `json:"persistence"`
}

type sessionPatch struct {
	Name *string `json:"name,omitempty"`
	Cols *int    `json:"cols,omitempty"`
	Rows *int    `json:"rows,omitempty"`
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
	if v.Name == "" || len(v.Name) > 80 {
		return errors.New("主机名称不能为空且不能超过 80 个字符")
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
	case "password":
		if v.Password == nil || *v.Password == "" {
			return errors.New("密码认证需要填写密码")
		}
	case "privateKey":
		if v.PrivateKey == nil || strings.TrimSpace(*v.PrivateKey) == "" {
			return errors.New("私钥认证需要填写私钥")
		}
	case "agent":
	default:
		return errors.New("不支持的 SSH 认证方式")
	}
	return nil
}

func (v *sessionInput) normalize() error {
	v.Name = strings.TrimSpace(v.Name)
	v.Kind = strings.TrimSpace(v.Kind)
	v.HostID = strings.TrimSpace(v.HostID)
	v.Cwd = strings.TrimSpace(v.Cwd)
	v.Command = strings.TrimSpace(v.Command)
	v.Persistence = strings.TrimSpace(v.Persistence)
	if len(v.Name) > 80 {
		return errors.New("会话名称不能超过 80 个字符")
	}
	if v.Kind != "local" && v.Kind != "ssh" {
		return errors.New("会话类型必须是 local 或 ssh")
	}
	if v.Kind == "ssh" && v.HostID == "" {
		return errors.New("SSH 会话必须选择主机")
	}
	if v.Kind == "local" {
		v.HostID = ""
	}
	if len(v.Cwd) > 4096 || len(v.Command) > 8192 {
		return errors.New("工作目录或启动命令过长")
	}
	if v.Persistence == "" {
		v.Persistence = "auto"
	}
	switch v.Persistence {
	case "auto", "tmux", "screen", "none":
	default:
		return errors.New("不支持的持久化模式")
	}
	return nil
}

func validSize(cols, rows int) bool {
	return cols >= 20 && cols <= 1000 && rows >= 5 && rows <= 500
}

func newID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(value), nil
}

func sshAddress(address string, port int) string {
	return net.JoinHostPort(strings.Trim(address, "[]"), fmt.Sprint(port))
}
