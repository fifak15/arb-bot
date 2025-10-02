package main

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseFlags_RunDiscovery(t *testing.T) {
	// Temporarily modify os.Args to simulate command-line flags
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()
	os.Args = []string{"cmd", "--run-discovery=true"}

	// Reset flags to avoid interference between tests
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	// Call the function that parses flags
	_, _, _, _, runDiscovery := parseFlags()

	// Assert that runDiscovery is true
	assert.True(t, runDiscovery, "Expected runDiscovery to be true, but it was false")
}