package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// findSubcommand returns the named subcommand of `inject`, or nil.
func findSubcommand(name string) *cobra.Command {
	for _, c := range injectCmd.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// TestInjectStatefulSetCommandIsRegistered is #91 at the level the user meets
// it: `ktunnel inject statefulset owl-app 9003` has to be a command that
// exists. Everything under it can be right and still be unreachable if the
// subcommand is never added to `inject`, which is one line in init() and
// nothing else in the package would notice.
func TestInjectStatefulSetCommandIsRegistered(t *testing.T) {
	cmd := findSubcommand("statefulset")
	if cmd == nil {
		t.Fatal("`ktunnel inject statefulset` is not a command; #91's user still has nothing to type")
	}

	// Two arguments minimum -- the object and at least one port -- the same
	// as its sibling. Accepting one would take the workload name and then
	// tunnel nothing.
	if err := cmd.Args(cmd, []string{"owl-app"}); err == nil {
		t.Error("`inject statefulset NAME` with no ports was accepted; there is nothing to tunnel")
	}
	if err := cmd.Args(cmd, []string{"owl-app", "9003"}); err != nil {
		t.Errorf("`inject statefulset owl-app 9003` was rejected: %v", err)
	}
}

// TestInjectStatefulSetTakesTheSameFlagsAsDeployment: the flags were
// registered by hand per subcommand, and a copied block is exactly the kind of
// thing that drifts -- a missing --namespace or --eject on the new subcommand
// is a command that compiles, runs, and quietly does the wrong thing in the
// wrong namespace.
func TestInjectStatefulSetTakesTheSameFlagsAsDeployment(t *testing.T) {
	sts, deployment := findSubcommand("statefulset"), findSubcommand("deployment")
	if sts == nil || deployment == nil {
		t.Fatal("both inject subcommands must exist")
	}

	deployment.Flags().VisitAll(func(f *pflag.Flag) {
		if sts.Flags().Lookup(f.Name) == nil {
			t.Errorf("`inject statefulset` has no --%s, which `inject deployment` takes", f.Name)
		}
	})
}

// TestInjectStatefulSetHelpSaysWhatIsDifferent: the ordinal-to-port mapping is
// the one thing this subcommand does that its sibling does not, and a user
// attaching a debugger to a particular pod has to be able to find out which
// port that pod is on without reading the source.
func TestInjectStatefulSetHelpSaysWhatIsDifferent(t *testing.T) {
	cmd := findSubcommand("statefulset")
	if cmd == nil {
		t.Fatal("`ktunnel inject statefulset` is not a command")
	}
	for _, want := range []string{"ordinal", "OnDelete"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("the help does not mention %q, which is behaviour a user cannot guess", want)
		}
	}
}
