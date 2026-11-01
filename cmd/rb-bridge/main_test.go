package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

func TestStartupValidationPrecedesImportAndWorkers(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--watch", "--prolink", "--bind=invalid"}, "bind must be IPv4"},
		{[]string{"--mount-path=/missing-library", "--prolink", "--bind=::1"}, "bind must be IPv4"},
		{[]string{"--watch", "--watch-roots="}, "watch roots must not be empty"},
		{[]string{"--watch", "--watch-roots=/media, ,/mnt"}, "watch roots must not be empty"},
	} {
		oldArgs, oldFlags := os.Args, flag.CommandLine
		os.Args = append([]string{"rb-bridge"}, tc.args...)
		flag.CommandLine = flag.NewFlagSet("rb-bridge", flag.ContinueOnError)
		flag.CommandLine.SetOutput(io.Discard)
		err := run()
		os.Args, flag.CommandLine = oldArgs, oldFlags
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v, want %s", tc.args, err, tc.want)
		}
	}
}
