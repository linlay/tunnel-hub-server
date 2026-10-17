package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDesktopReactivationPreservesRegistration(t *testing.T) {
	db := openTestDB(t)
	input := RegisterDesktopDeviceInput{OwnerUserID: "owner", DeviceID: "computer", DeviceName: "Original", PublicHost: "original.m.example.test"}
	first, err := db.RegisterDesktopDevice(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RenameAccountDesktop(t.Context(), "owner", "computer", "Custom Name"); err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeAccountDesktop(t.Context(), "owner", "computer"); err != nil {
		t.Fatal(err)
	}
	devices, err := db.ListDesktopDevices(t.Context())
	if err != nil || len(devices) != 1 || devices[0].RevokedAt == nil {
		t.Fatal("missing revocation", err)
	}
	revokedAt := *devices[0].RevokedAt
	for _, started := range []time.Time{{}, revokedAt.Add(-time.Hour), revokedAt} {
		input.SessionStartedAt = started
		if _, err := db.RegisterDesktopDevice(t.Context(), input); !errors.Is(err, ErrDesktopDeviceRevoked) {
			t.Fatal("old login restored device", err)
		}
	}
	input.SessionStartedAt = revokedAt.Add(time.Millisecond)
	input.PublicHost = "replacement.m.example.test"
	for range 10 {
		result, err := db.RegisterDesktopDevice(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		if result.Created || result.Device.DeviceKey != first.Device.DeviceKey || result.Device.PublicHost != first.Device.PublicHost || result.Device.DeviceName != "Custom Name" || result.Device.RevokedAt != nil {
			t.Fatal("registration identity or metadata changed")
		}
	}
	devices, err = db.ListDesktopDevices(t.Context())
	if err != nil || len(devices) != 1 {
		t.Fatal("duplicate records", err)
	}
}

func TestConcurrentRevocationCannotBeUndoneByOldLogin(t *testing.T) {
	db := openTestDB(t)
	input := RegisterDesktopDeviceInput{OwnerUserID: "owner", DeviceID: "computer", PublicHost: "computer.m.example.test", SessionStartedAt: time.Now().Add(-time.Hour)}
	if _, err := db.RegisterDesktopDevice(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		errorsFound <- db.RevokeAccountDesktop(t.Context(), "owner", "computer")
	}()
	go func() {
		defer workers.Done()
		<-start
		_, err := db.RegisterDesktopDevice(t.Context(), input)
		errorsFound <- err
	}()
	close(start)
	workers.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil && !errors.Is(err, ErrDesktopDeviceRevoked) {
			t.Fatal(err)
		}
	}
	if _, err := db.GetDesktopDeviceByOwnerAndID(t.Context(), "owner", "computer"); !errors.Is(err, ErrNotFound) {
		t.Fatal("old login bypassed concurrent revocation", err)
	}
}
