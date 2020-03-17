package gui

import (
	"fmt"
	"strings"

	"github.com/jesseduffield/gocui"
	"github.com/jesseduffield/lazygit/pkg/commands"
	"github.com/jesseduffield/lazygit/pkg/gui/presentation"
	"github.com/jesseduffield/lazygit/pkg/utils"
)

// list panel functions

func (gui *Gui) getSelectedBranch() *commands.Branch {
	selectedLine := gui.State.Panels.Branches.SelectedLine
	if selectedLine == -1 {
		return nil
	}

	return gui.State.Branches[selectedLine]
}

// may want to standardise how these select methods work
func (gui *Gui) handleBranchSelect(g *gocui.Gui, v *gocui.View) error {
	if gui.popupPanelFocused() {
		return nil
	}

	gui.State.SplitMainPanel = false

	if _, err := gui.g.SetCurrentView(v.Name()); err != nil {
		return err
	}

	gui.getMainView().Title = "Log"

	// This really shouldn't happen: there should always be a master branch
	if len(gui.State.Branches) == 0 {
		return gui.newStringTask("main", gui.Tr.SLocalize("NoBranchesThisRepo"))
	}
	branch := gui.getSelectedBranch()
	v.FocusPoint(0, gui.State.Panels.Branches.SelectedLine)

	cmd := gui.OSCommand.ExecutableFromString(
		gui.GitCommand.GetBranchGraphCmdStr(branch.Name),
	)
	if err := gui.newCmdTask("main", cmd); err != nil {
		gui.Log.Error(err)
	}
	return nil
}

// gui.refreshStatus is called at the end of this because that's when we can
// be sure there is a state.Branches array to pick the current branch from
func (gui *Gui) refreshBranches(g *gocui.Gui) error {
	if err := gui.refreshRemotes(); err != nil {
		return err
	}

	if err := gui.refreshTags(); err != nil {
		return err
	}

	g.Update(func(g *gocui.Gui) error {
		builder, err := commands.NewBranchListBuilder(gui.Log, gui.GitCommand)
		if err != nil {
			return err
		}
		gui.State.Branches = builder.Build()

		// TODO: if we're in the remotes view and we've just deleted a remote we need to refresh accordingly
		if gui.getBranchesView().Context == "local-branches" {
			if err := gui.renderLocalBranchesWithSelection(); err != nil {
				return err
			}
		}

		return gui.refreshStatus(g)
	})
	return nil
}

func (gui *Gui) renderLocalBranchesWithSelection() error {
	branchesView := gui.getBranchesView()

	gui.refreshSelectedLine(&gui.State.Panels.Branches.SelectedLine, len(gui.State.Branches))
	displayStrings := presentation.GetBranchListDisplayStrings(gui.State.Branches, gui.State.ScreenMode != SCREEN_NORMAL)
	gui.renderDisplayStrings(branchesView, displayStrings)
	if gui.g.CurrentView() == branchesView {
		if err := gui.handleBranchSelect(gui.g, branchesView); err != nil {
			return err
		}
	}

	return nil
}

// specific functions

func (gui *Gui) handleBranchPress(g *gocui.Gui, v *gocui.View) error {
	if gui.State.Panels.Branches.SelectedLine == -1 {
		return nil
	}
	if gui.State.Panels.Branches.SelectedLine == 0 {
		return gui.createErrorPanel(g, gui.Tr.SLocalize("AlreadyCheckedOutBranch"))
	}
	branch := gui.getSelectedBranch()
	return gui.handleCheckoutRef(branch.Name)
}

func (gui *Gui) handleCreatePullRequestPress(g *gocui.Gui, v *gocui.View) error {
	pullRequest := commands.NewPullRequest(gui.GitCommand)

	branch := gui.getSelectedBranch()
	if err := pullRequest.Create(branch); err != nil {
		return gui.createErrorPanel(g, err.Error())
	}

	return nil
}

func (gui *Gui) handleGitFetch(g *gocui.Gui, v *gocui.View) error {
	if err := gui.createLoaderPanel(gui.g, v, gui.Tr.SLocalize("FetchWait")); err != nil {
		return err
	}
	go func() {
		unamePassOpend, err := gui.fetch(g, v, true)
		gui.HandleCredentialsPopup(g, unamePassOpend, err)
	}()
	return nil
}

func (gui *Gui) handleForceCheckout(g *gocui.Gui, v *gocui.View) error {
	branch := gui.getSelectedBranch()
	message := gui.Tr.SLocalize("SureForceCheckout")
	title := gui.Tr.SLocalize("ForceCheckoutBranch")
	return gui.createConfirmationPanel(g, v, true, title, message, func(g *gocui.Gui, v *gocui.View) error {
		if err := gui.GitCommand.Checkout(branch.Name, true); err != nil {
			_ = gui.createErrorPanel(g, err.Error())
		}
		return gui.refreshSidePanels(g)
	}, nil)
}

func (gui *Gui) handleCheckoutRef(ref string) error {
	if err := gui.GitCommand.Checkout(ref, false); err != nil {
		// note, this will only work for english-language git commands. If we force git to use english, and the error isn't this one, then the user will receive an english command they may not understand. I'm not sure what the best solution to this is. Running the command once in english and a second time in the native language is one option

		if strings.Contains(err.Error(), "Please commit your changes or stash them before you switch branch") {
			// offer to autostash changes
			return gui.createConfirmationPanel(gui.g, gui.getBranchesView(), true, gui.Tr.SLocalize("AutoStashTitle"), gui.Tr.SLocalize("AutoStashPrompt"), func(g *gocui.Gui, v *gocui.View) error {
				if err := gui.GitCommand.StashSave(gui.Tr.SLocalize("StashPrefix") + ref); err != nil {
					return gui.createErrorPanel(g, err.Error())
				}
				if err := gui.GitCommand.Checkout(ref, false); err != nil {
					return gui.createErrorPanel(g, err.Error())
				}

				// checkout successful so we select the new branch
				gui.State.Panels.Branches.SelectedLine = 0

				if err := gui.GitCommand.StashDo(0, "pop"); err != nil {
					if err := gui.refreshSidePanels(g); err != nil {
						return err
					}
					return gui.createErrorPanel(g, err.Error())
				}
				return gui.refreshSidePanels(g)
			}, nil)
		}

		if err := gui.createErrorPanel(gui.g, err.Error()); err != nil {
			return err
		}
	}

	gui.State.Panels.Branches.SelectedLine = 0
	gui.State.Panels.Commits.SelectedLine = 0
	return gui.refreshSidePanels(gui.g)
}

func (gui *Gui) handleCheckoutByName(g *gocui.Gui, v *gocui.View) error {
	return gui.createPromptPanel(g, v, gui.Tr.SLocalize("BranchName")+":", "", func(g *gocui.Gui, v *gocui.View) error {
		return gui.handleCheckoutRef(gui.trimmedContent(v))
	})
}

func (gui *Gui) getCheckedOutBranch() *commands.Branch {
	if len(gui.State.Branches) == 0 {
		return nil
	}

	return gui.State.Branches[0]
}

func (gui *Gui) handleNewBranch(g *gocui.Gui, v *gocui.View) error {
	branch := gui.getSelectedBranch()
	if branch == nil {
		return nil
	}
	message := gui.Tr.TemplateLocalize(
		"NewBranchNameBranchOff",
		Teml{
			"branchName": branch.Name,
		},
	)
	return gui.createPromptPanel(g, v, message, "", func(g *gocui.Gui, v *gocui.View) error {
		if err := gui.GitCommand.NewBranch(gui.trimmedContent(v), branch.Name); err != nil {
			return gui.createErrorPanel(g, err.Error())
		}
		if err := gui.refreshSidePanels(g); err != nil {
			return gui.createErrorPanel(g, err.Error())
		}
		return gui.handleBranchSelect(g, v)
	})
}

func (gui *Gui) handleDeleteBranch(g *gocui.Gui, v *gocui.View) error {
	return gui.deleteBranch(g, v, false)
}

func (gui *Gui) deleteBranch(g *gocui.Gui, v *gocui.View, force bool) error {
	selectedBranch := gui.getSelectedBranch()
	if selectedBranch == nil {
		return nil
	}
	checkedOutBranch := gui.getCheckedOutBranch()
	if checkedOutBranch.Name == selectedBranch.Name {
		return gui.createErrorPanel(g, gui.Tr.SLocalize("CantDeleteCheckOutBranch"))
	}
	return gui.deleteNamedBranch(g, v, selectedBranch, force)
}

func (gui *Gui) deleteNamedBranch(g *gocui.Gui, v *gocui.View, selectedBranch *commands.Branch, force bool) error {
	title := gui.Tr.SLocalize("DeleteBranch")
	var messageID string
	if force {
		messageID = "ForceDeleteBranchMessage"
	} else {
		messageID = "DeleteBranchMessage"
	}
	message := gui.Tr.TemplateLocalize(
		messageID,
		Teml{
			"selectedBranchName": selectedBranch.Name,
		},
	)
	return gui.createConfirmationPanel(g, v, true, title, message, func(g *gocui.Gui, v *gocui.View) error {
		if err := gui.GitCommand.DeleteBranch(selectedBranch.Name, force); err != nil {
			errMessage := err.Error()
			if !force && strings.Contains(errMessage, "is not fully merged") {
				return gui.deleteNamedBranch(g, v, selectedBranch, true)
			}
			return gui.createErrorPanel(g, errMessage)
		}
		return gui.refreshSidePanels(g)
	}, nil)
}

func (gui *Gui) mergeBranchIntoCheckedOutBranch(branchName string) error {
	if gui.GitCommand.IsHeadDetached() {
		return gui.createErrorPanel(gui.g, "Cannot merge branch in detached head state. You might have checked out a commit directly or a remote branch, in which case you should checkout the local branch you want to be on")
	}
	checkedOutBranchName := gui.getCheckedOutBranch().Name
	if checkedOutBranchName == branchName {
		return gui.createErrorPanel(gui.g, gui.Tr.SLocalize("CantMergeBranchIntoItself"))
	}
	prompt := gui.Tr.TemplateLocalize(
		"ConfirmMerge",
		Teml{
			"checkedOutBranch": checkedOutBranchName,
			"selectedBranch":   branchName,
		},
	)
	return gui.createConfirmationPanel(gui.g, gui.getBranchesView(), true, gui.Tr.SLocalize("MergingTitle"), prompt,
		func(g *gocui.Gui, v *gocui.View) error {

			err := gui.GitCommand.Merge(branchName)
			return gui.handleGenericMergeCommandResult(err)
		}, nil)
}

func (gui *Gui) handleMerge(g *gocui.Gui, v *gocui.View) error {
	selectedBranchName := gui.getSelectedBranch().Name
	return gui.mergeBranchIntoCheckedOutBranch(selectedBranchName)
}

func (gui *Gui) handleRebaseOntoLocalBranch(g *gocui.Gui, v *gocui.View) error {
	selectedBranchName := gui.getSelectedBranch().Name
	return gui.handleRebaseOntoBranch(selectedBranchName)
}

func (gui *Gui) handleRebaseOntoBranch(selectedBranchName string) error {
	checkedOutBranch := gui.getCheckedOutBranch().Name
	if selectedBranchName == checkedOutBranch {
		return gui.createErrorPanel(gui.g, gui.Tr.SLocalize("CantRebaseOntoSelf"))
	}
	prompt := gui.Tr.TemplateLocalize(
		"ConfirmRebase",
		Teml{
			"checkedOutBranch": checkedOutBranch,
			"selectedBranch":   selectedBranchName,
		},
	)
	return gui.createConfirmationPanel(gui.g, gui.getBranchesView(), true, gui.Tr.SLocalize("RebasingTitle"), prompt,
		func(g *gocui.Gui, v *gocui.View) error {
			err := gui.GitCommand.RebaseBranch(selectedBranchName)
			return gui.handleGenericMergeCommandResult(err)
		}, nil)
}

func (gui *Gui) handleFastForward(g *gocui.Gui, v *gocui.View) error {
	branch := gui.getSelectedBranch()
	if branch == nil {
		return nil
	}
	if branch.Pushables == "" {
		return nil
	}
	if branch.Pushables == "?" {
		return gui.createErrorPanel(gui.g, gui.Tr.SLocalize("FwdNoUpstream"))
	}
	if branch.Pushables != "0" {
		return gui.createErrorPanel(gui.g, gui.Tr.SLocalize("FwdCommitsToPush"))
	}

	upstream, err := gui.GitCommand.GetUpstreamForBranch(branch.Name)
	if err != nil {
		return gui.createErrorPanel(gui.g, err.Error())
	}

	split := strings.Split(upstream, "/")
	remoteName := split[0]
	remoteBranchName := strings.Join(split[1:], "/")

	message := gui.Tr.TemplateLocalize(
		"Fetching",
		Teml{
			"from": fmt.Sprintf("%s/%s", remoteName, remoteBranchName),
			"to":   branch.Name,
		},
	)
	go func() {
		_ = gui.createLoaderPanel(gui.g, v, message)

		if gui.State.Panels.Branches.SelectedLine == 0 {
			if err := gui.GitCommand.PullWithoutPasswordCheck("--ff-only"); err != nil {
				_ = gui.createErrorPanel(gui.g, err.Error())
			}
			_ = gui.refreshSidePanels(gui.g)
		} else {
			if err := gui.GitCommand.FastForward(branch.Name, remoteName, remoteBranchName); err != nil {
				_ = gui.createErrorPanel(gui.g, err.Error())
			}
			_ = gui.refreshBranches(gui.g)
		}

		_ = gui.closeConfirmationPrompt(gui.g, true)
	}()
	return nil
}

func (gui *Gui) onBranchesTabClick(tabIndex int) error {
	contexts := []string{"local-branches", "remotes", "tags"}
	branchesView := gui.getBranchesView()
	branchesView.TabIndex = tabIndex

	return gui.switchBranchesPanelContext(contexts[tabIndex])
}

func (gui *Gui) switchBranchesPanelContext(context string) error {
	branchesView := gui.getBranchesView()
	branchesView.Context = context
	if err := gui.onSearchEscape(); err != nil {
		return err
	}

	contextTabIndexMap := map[string]int{
		"local-branches":  0,
		"remotes":         1,
		"remote-branches": 1,
		"tags":            2,
	}

	branchesView.TabIndex = contextTabIndexMap[context]

	return gui.refreshBranchesViewWithSelection()
}

func (gui *Gui) refreshBranchesViewWithSelection() error {
	branchesView := gui.getBranchesView()

	switch branchesView.Context {
	case "local-branches":
		return gui.renderLocalBranchesWithSelection()
	case "remotes":
		return gui.renderRemotesWithSelection()
	case "remote-branches":
		return gui.renderRemoteBranchesWithSelection()
	case "tags":
		return gui.renderTagsWithSelection()
	}

	return nil
}

func (gui *Gui) handleNextBranchesTab(g *gocui.Gui, v *gocui.View) error {
	return gui.onBranchesTabClick(
		utils.ModuloWithWrap(v.TabIndex+1, len(v.Tabs)),
	)
}

func (gui *Gui) handlePrevBranchesTab(g *gocui.Gui, v *gocui.View) error {
	return gui.onBranchesTabClick(
		utils.ModuloWithWrap(v.TabIndex-1, len(v.Tabs)),
	)
}

func (gui *Gui) handleCreateResetToBranchMenu(g *gocui.Gui, v *gocui.View) error {
	branch := gui.getSelectedBranch()
	if branch == nil {
		return nil
	}

	return gui.createResetMenu(branch.Name)
}

func (gui *Gui) onBranchesPanelSearchSelect(selectedLine int) error {
	branchesView := gui.getBranchesView()
	switch branchesView.Context {
	case "local-branches":
		gui.State.Panels.Branches.SelectedLine = selectedLine
		return gui.handleBranchSelect(gui.g, branchesView)
	case "remotes":
		gui.State.Panels.Remotes.SelectedLine = selectedLine
		return gui.handleRemoteSelect(gui.g, branchesView)
	case "remote-branches":
		gui.State.Panels.RemoteBranches.SelectedLine = selectedLine
		return gui.handleRemoteBranchSelect(gui.g, branchesView)
	}
	return nil
}

func (gui *Gui) handleRenameBranch(g *gocui.Gui, v *gocui.View) error {
	branch := gui.getSelectedBranch()
	if branch == nil {
		return nil
	}

	promptForNewName := func() error {
		return gui.createPromptPanel(g, v, gui.Tr.SLocalize("NewBranchNamePrompt")+" "+branch.Name+":", "", func(g *gocui.Gui, v *gocui.View) error {
			newName := gui.trimmedContent(v)
			if err := gui.GitCommand.RenameBranch(branch.Name, newName); err != nil {
				return gui.createErrorPanel(gui.g, err.Error())
			}
			// need to checkout so that the branch shows up in our reflog and therefore
			// doesn't get lost among all the other branches when we switch to something else
			if err := gui.GitCommand.Checkout(newName, false); err != nil {
				return gui.createErrorPanel(gui.g, err.Error())
			}

			return gui.refreshBranches(gui.g)
		})
	}

	// I could do an explicit check here for whether the branch is tracking a remote branch
	// but if we've selected it we'll already know that via Pullables and Pullables.
	// Bit of a hack but I'm lazy.
	notTrackingRemote := branch.Pullables == "?"
	if notTrackingRemote {
		return promptForNewName()
	}
	return gui.createConfirmationPanel(gui.g, v, true, gui.Tr.SLocalize("renameBranch"), gui.Tr.SLocalize("RenameBranchWarning"), func(_g *gocui.Gui, _v *gocui.View) error {
		return promptForNewName()
	}, nil)
}

func (gui *Gui) currentBranch() *commands.Branch {
	return gui.State.Branches[0]
}
