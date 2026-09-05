package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestHostCRUD(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	host, err := s.CreateHost(ctx, Host{
		Name:                 "Production",
		Address:              "server.example.test",
		Port:                 22,
		Username:             "deploy",
		AuthType:             HostAuthKey,
		EncryptedCredentials: []byte{1, 2, 3},
		Fingerprint:          "SHA256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHost(ctx, host.ID)
	if err != nil || got.Name != "Production" || !bytes.Equal(got.EncryptedCredentials, []byte{1, 2, 3}) {
		t.Fatalf("GetHost = %+v, %v", got, err)
	}
	host.Name = "Prod renamed"
	host.Port = 2222
	host.EncryptedCredentials = []byte{4, 5}
	now = now.Add(time.Minute)
	host, err = s.UpdateHost(ctx, host)
	if err != nil || host.Name != "Prod renamed" || host.Port != 2222 || !host.UpdatedAt.Equal(now) {
		t.Fatalf("UpdateHost = %+v, %v", host, err)
	}
	hosts, err := s.ListHosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].ID != host.ID {
		t.Fatalf("ListHosts = %+v, %v", hosts, err)
	}
	if err := s.DeleteHost(ctx, host.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetHost(ctx, host.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted GetHost error = %v", err)
	}
	if err := s.DeleteHost(ctx, host.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteHost error = %v", err)
	}
}

func TestUpdateHostFingerprintOnlyTouchesTheFingerprint(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	s := openTestStore(t, &now)
	ctx := context.Background()
	host, err := s.CreateHost(ctx, Host{
		Name: "Box", Address: "10.0.0.9", Port: 22, Username: "me",
		AuthType: HostAuthPassword, EncryptedCredentials: []byte("sealed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := s.UpdateHostFingerprint(ctx, host.ID, "SHA256:trusted"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetHost(ctx, host.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "SHA256:trusted" || got.Name != "Box" || string(got.EncryptedCredentials) != "sealed" {
		t.Fatalf("fingerprint update changed another field: %+v", got)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Fatalf("fingerprint update did not touch UpdatedAt: %v", got.UpdatedAt)
	}
	if err := s.UpdateHostFingerprint(ctx, "missing", "SHA256:x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing host fingerprint update = %v, want ErrNotFound", err)
	}
}
