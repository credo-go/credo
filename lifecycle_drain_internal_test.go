package credo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestLifecycleManagerRunDrainPhaseJoinsHTTPAndHookErrors(t *testing.T) {
	httpErr := errors.New("HTTP drain failed")
	hookErr := errors.New("subsystem drain failed")
	lm := &lifecycleManager{
		onDrain: []drainHook{{
			index:  0,
			source: "test registration",
			fn: func(context.Context) error {
				return hookErr
			},
		}},
	}

	err := lm.runDrainPhase(t.Context(), func(context.Context) error {
		return httpErr
	})
	if !errors.Is(err, httpErr) {
		t.Errorf("drain error = %v, want HTTP cause", err)
	}
	if !errors.Is(err, hookErr) {
		t.Errorf("drain error = %v, want hook cause", err)
	}
}

func TestDrainHTTPServersPreservesRedirectBeforeMainOrdering(t *testing.T) {
	redirectStarted := make(chan struct{})
	mainStarted := make(chan struct{})
	releaseRedirect := make(chan struct{})
	releaseMain := make(chan struct{})
	var redirectOnce, mainOnce sync.Once
	var releaseRedirectOnce, releaseMainOnce sync.Once

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectOnce.Do(func() { close(redirectStarted) })
		<-releaseRedirect
		w.WriteHeader(http.StatusNoContent)
	}))
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mainOnce.Do(func() { close(mainStarted) })
		<-releaseMain
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() {
		releaseRedirectOnce.Do(func() { close(releaseRedirect) })
		releaseMainOnce.Do(func() { close(releaseMain) })
		redirect.Close()
		main.Close()
	})

	requestDone := make(chan error, 2)
	go func() {
		resp, err := redirect.Client().Get(redirect.URL)
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	go func() {
		resp, err := main.Client().Get(main.URL)
		if resp != nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	waitDrainTestSignal(t, redirectStarted, "redirect handler")
	waitDrainTestSignal(t, mainStarted, "main handler")

	redirectShutdown := make(chan struct{})
	mainShutdown := make(chan struct{})
	redirect.Config.RegisterOnShutdown(func() { close(redirectShutdown) })
	main.Config.RegisterOnShutdown(func() { close(mainShutdown) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	drainDone := make(chan error, 1)
	go func() {
		drainDone <- drainHTTPServers(ctx, redirect.Config, main.Config)
	}()

	waitDrainTestSignal(t, redirectShutdown, "redirect shutdown")
	select {
	case <-mainShutdown:
		t.Fatal("main shutdown began before redirect shutdown completed")
	default:
	}
	releaseRedirectOnce.Do(func() { close(releaseRedirect) })
	waitDrainTestSignal(t, mainShutdown, "main shutdown")
	releaseMainOnce.Do(func() { close(releaseMain) })

	if err := <-drainDone; err != nil {
		t.Fatalf("drainHTTPServers() error: %v", err)
	}
	for range 2 {
		if err := <-requestDone; err != nil {
			t.Fatalf("in-flight request error: %v", err)
		}
	}
}

func waitDrainTestSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", label)
	}
}
