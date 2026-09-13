package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// gateContinue mirrors Omega's own gate: the user must literally type
// CONTINUE. Piped input works the same way.
func gateContinue(cmd *cobra.Command) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "%s", "type CONTINUE and press Enter to run the restore (anything else exits): ")
	var line string
	if _, err := fmt.Fscanln(cmd.InOrStdin(), &line); err != nil {
		return false
	}
	return strings.ToUpper(strings.TrimSpace(line)) == "CONTINUE"
}
