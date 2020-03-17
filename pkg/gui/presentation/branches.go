package presentation

import (
	"fmt"
	"strings"

	"github.com/fatih/color"
	"github.com/jesseduffield/lazygit/pkg/commands"
	"github.com/jesseduffield/lazygit/pkg/theme"
	"github.com/jesseduffield/lazygit/pkg/utils"
)

func GetBranchListDisplayStrings(branches []*commands.Branch, fullDescription bool) [][]string {
	lines := make([][]string, len(branches))

	for i := range branches {
		lines[i] = getBranchDisplayStrings(branches[i], fullDescription)
	}

	return lines
}

// getBranchDisplayStrings returns the display string of branch
func getBranchDisplayStrings(b *commands.Branch, fullDescription bool) []string {
	displayName := utils.ColoredString(b.Name, GetBranchColor(b.Name))
	if b.Pushables != "" && b.Pullables != "" && b.Pushables != "?" && b.Pullables != "?" {
		trackColor := color.FgYellow
		if b.Pushables == "0" && b.Pullables == "0" {
			trackColor = color.FgGreen
		}
		track := utils.ColoredString(fmt.Sprintf("↑%s↓%s", b.Pushables, b.Pullables), trackColor)
		displayName = fmt.Sprintf("%s %s", displayName, track)
	}

	recencyColor := color.FgCyan
	if b.Recency == "  *" {
		recencyColor = color.FgGreen
	}

	if fullDescription {
		return []string{utils.ColoredString(b.Recency, recencyColor), displayName, utils.ColoredString(b.UpstreamName, color.FgYellow)}
	}

	return []string{utils.ColoredString(b.Recency, recencyColor), displayName}
}

// GetBranchColor branch color
func GetBranchColor(name string) color.Attribute {
	branchType := strings.Split(name, "/")[0]

	switch branchType {
	case "feature":
		return color.FgGreen
	case "bugfix":
		return color.FgYellow
	case "hotfix":
		return color.FgRed
	default:
		return theme.DefaultTextColor
	}
}
