package main

import (
	"context"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/cloudapi"
)

func TestShutdownStopsLocalHTTPServers(t *testing.T) {
	app := NewApp()
	app.cloudAPI = cloudapi.New(func(string, map[string]any) (any, error) { return nil, nil })
	if err := app.cloudAPI.Start(0); err != nil {
		t.Fatalf("start cloudapi: %v", err)
	}
	if !app.cloudAPI.Running() {
		t.Fatal("cloudapi did not start")
	}

	app.shutdown(context.Background())

	if app.cloudAPI.Running() {
		t.Fatal("shutdown left cloudapi listening")
	}
}
