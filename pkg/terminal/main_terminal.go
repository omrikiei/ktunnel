package terminal

import (
	"context"
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
	"os"
	"strings"
)

func RunTUI(ctx context.Context) error {
	for {
		select {
		case <- ctx.Done():
				return nil
		default:

			physicalWidth, _, _ := term.GetSize(int(os.Stdout.Fd()))
			doc := strings.Builder{}

			// Tabs
			{
				row := lipgloss.JoinHorizontal(
					lipgloss.Top,
					activeTab.Render("Main"),
					tab.Render("Logs"),
				)
				gap := tabGap.Render(strings.Repeat(" ", max(0, width-lipgloss.Width(row)-2)))
				row = lipgloss.JoinHorizontal(lipgloss.Bottom, row, gap)
				doc.WriteString(row + "\n\n")
			}

			if physicalWidth > 0 {
				docStyle = docStyle.MaxWidth(physicalWidth)
			}

			// Okay, let's print it
			fmt.Println(docStyle.Render(doc.String()))
		}
	}
}