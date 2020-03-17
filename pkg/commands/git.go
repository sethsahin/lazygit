package commands

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgutz/str"

	"github.com/go-errors/errors"

	"github.com/jesseduffield/lazygit/pkg/config"
	"github.com/jesseduffield/lazygit/pkg/i18n"
	"github.com/jesseduffield/lazygit/pkg/utils"
	"github.com/sirupsen/logrus"
	gitconfig "github.com/tcnksm/go-gitconfig"
	gogit "gopkg.in/src-d/go-git.v4"
)

// this takes something like:
// * (HEAD detached at 264fc6f5)
//	remotes
// and returns '264fc6f5' as the second match
const CurrentBranchNameRegex = `(?m)^\*.*?([^ ]*?)\)?$`

func verifyInGitRepo(runCmd func(string, ...interface{}) error) error {
	return runCmd("git status")
}

func navigateToRepoRootDirectory(stat func(string) (os.FileInfo, error), chdir func(string) error) error {
	for {
		_, err := stat(".git")

		if err == nil {
			return nil
		}

		if !os.IsNotExist(err) {
			return WrapError(err)
		}

		if err = chdir(".."); err != nil {
			return WrapError(err)
		}
	}
}

func setupRepositoryAndWorktree(openGitRepository func(string) (*gogit.Repository, error), sLocalize func(string) string) (repository *gogit.Repository, worktree *gogit.Worktree, err error) {
	repository, err = openGitRepository(".")

	if err != nil {
		if strings.Contains(err.Error(), `unquoted '\' must be followed by new line`) {
			return nil, nil, errors.New(sLocalize("GitconfigParseErr"))
		}

		return
	}

	worktree, err = repository.Worktree()

	if err != nil {
		return
	}

	return
}

// GitCommand is our main git interface
type GitCommand struct {
	Log                  *logrus.Entry
	OSCommand            *OSCommand
	Worktree             *gogit.Worktree
	Repo                 *gogit.Repository
	Tr                   *i18n.Localizer
	Config               config.AppConfigurer
	getGlobalGitConfig   func(string) (string, error)
	getLocalGitConfig    func(string) (string, error)
	removeFile           func(string) error
	DotGitDir            string
	onSuccessfulContinue func() error
	PatchManager         *PatchManager
}

// NewGitCommand it runs git commands
func NewGitCommand(log *logrus.Entry, osCommand *OSCommand, tr *i18n.Localizer, config config.AppConfigurer) (*GitCommand, error) {
	var worktree *gogit.Worktree
	var repo *gogit.Repository

	fs := []func() error{
		func() error {
			return verifyInGitRepo(osCommand.RunCommand)
		},
		func() error {
			return navigateToRepoRootDirectory(os.Stat, os.Chdir)
		},
		func() error {
			var err error
			repo, worktree, err = setupRepositoryAndWorktree(gogit.PlainOpen, tr.SLocalize)
			return err
		},
	}

	for _, f := range fs {
		if err := f(); err != nil {
			return nil, err
		}
	}

	dotGitDir, err := findDotGitDir(os.Stat, ioutil.ReadFile)
	if err != nil {
		return nil, err
	}

	gitCommand := &GitCommand{
		Log:                log,
		OSCommand:          osCommand,
		Tr:                 tr,
		Worktree:           worktree,
		Repo:               repo,
		Config:             config,
		getGlobalGitConfig: gitconfig.Global,
		getLocalGitConfig:  gitconfig.Local,
		removeFile:         os.RemoveAll,
		DotGitDir:          dotGitDir,
	}

	gitCommand.PatchManager = NewPatchManager(log, gitCommand.ApplyPatch)

	return gitCommand, nil
}

func findDotGitDir(stat func(string) (os.FileInfo, error), readFile func(filename string) ([]byte, error)) (string, error) {
	f, err := stat(".git")
	if err != nil {
		return "", err
	}

	if f.IsDir() {
		return ".git", nil
	}

	fileBytes, err := readFile(".git")
	if err != nil {
		return "", err
	}
	fileContent := string(fileBytes)
	if !strings.HasPrefix(fileContent, "gitdir: ") {
		return "", errors.New(".git is a file which suggests we are in a submodule but the file's contents do not contain a gitdir pointing to the actual .git directory")
	}
	return strings.TrimSpace(strings.TrimPrefix(fileContent, "gitdir: ")), nil
}

// GetStashEntries stash entries
func (c *GitCommand) GetStashEntries() []*StashEntry {
	// if we directly put this string in RunCommandWithOutput the compiler complains because it thinks it's a format string
	unescaped := "git stash list --pretty='%gs'"
	rawString, _ := c.OSCommand.RunCommandWithOutput(unescaped)
	stashEntries := []*StashEntry{}
	for i, line := range utils.SplitLines(rawString) {
		stashEntries = append(stashEntries, stashEntryFromLine(line, i))
	}
	return stashEntries
}

func stashEntryFromLine(line string, index int) *StashEntry {
	return &StashEntry{
		Name:          line,
		Index:         index,
		DisplayString: line,
	}
}

// GetStashEntryDiff stash diff
func (c *GitCommand) ShowStashEntryCmdStr(index int) string {
	return fmt.Sprintf("git stash show -p --color=%s stash@{%d}", c.colorArg(), index)
}

// GetStatusFiles git status files
func (c *GitCommand) GetStatusFiles() []*File {
	statusOutput, _ := c.GitStatus()
	statusStrings := utils.SplitLines(statusOutput)
	files := []*File{}

	for _, statusString := range statusStrings {
		change := statusString[0:2]
		stagedChange := change[0:1]
		unstagedChange := statusString[1:2]
		filename := c.OSCommand.Unquote(statusString[3:])
		untracked := utils.IncludesString([]string{"??", "A ", "AM"}, change)
		hasNoStagedChanges := utils.IncludesString([]string{" ", "U", "?"}, stagedChange)
		hasMergeConflicts := utils.IncludesString([]string{"DD", "AA", "UU", "AU", "UA", "UD", "DU"}, change)
		hasInlineMergeConflicts := utils.IncludesString([]string{"UU", "AA"}, change)

		file := &File{
			Name:                    filename,
			DisplayString:           statusString,
			HasStagedChanges:        !hasNoStagedChanges,
			HasUnstagedChanges:      unstagedChange != " ",
			Tracked:                 !untracked,
			Deleted:                 unstagedChange == "D" || stagedChange == "D",
			HasMergeConflicts:       hasMergeConflicts,
			HasInlineMergeConflicts: hasInlineMergeConflicts,
			Type:                    c.OSCommand.FileType(filename),
			ShortStatus:             change,
		}
		files = append(files, file)
	}
	return files
}

// StashDo modify stash
func (c *GitCommand) StashDo(index int, method string) error {
	return c.OSCommand.RunCommand("git stash %s stash@{%d}", method, index)
}

// StashSave save stash
// TODO: before calling this, check if there is anything to save
func (c *GitCommand) StashSave(message string) error {
	return c.OSCommand.RunCommand("git stash save %s", c.OSCommand.Quote(message))
}

// MergeStatusFiles merge status files
func (c *GitCommand) MergeStatusFiles(oldFiles, newFiles []*File) []*File {
	if len(oldFiles) == 0 {
		return newFiles
	}

	appendedIndexes := []int{}

	// retain position of files we already could see
	result := []*File{}
	for _, oldFile := range oldFiles {
		for newIndex, newFile := range newFiles {
			if oldFile.Name == newFile.Name {
				result = append(result, newFile)
				appendedIndexes = append(appendedIndexes, newIndex)
				break
			}
		}
	}

	// append any new files to the end
	for index, newFile := range newFiles {
		if !includesInt(appendedIndexes, index) {
			result = append(result, newFile)
		}
	}

	return result
}

func includesInt(list []int, a int) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

// ResetAndClean removes all unstaged changes and removes all untracked files
func (c *GitCommand) ResetAndClean() error {
	if err := c.ResetHard("HEAD"); err != nil {
		return err
	}

	return c.RemoveUntrackedFiles()
}

func (c *GitCommand) GetCurrentBranchUpstreamDifferenceCount() (string, string) {
	return c.GetCommitDifferences("HEAD", "HEAD@{u}")
}

func (c *GitCommand) GetBranchUpstreamDifferenceCount(branchName string) (string, string) {
	return c.GetCommitDifferences(branchName, branchName+"@{u}")
}

// GetCommitDifferences checks how many pushables/pullables there are for the
// current branch
func (c *GitCommand) GetCommitDifferences(from, to string) (string, string) {
	command := "git rev-list %s..%s --count"
	pushableCount, err := c.OSCommand.RunCommandWithOutput(command, to, from)
	if err != nil {
		return "?", "?"
	}
	pullableCount, err := c.OSCommand.RunCommandWithOutput(command, from, to)
	if err != nil {
		return "?", "?"
	}
	return strings.TrimSpace(pushableCount), strings.TrimSpace(pullableCount)
}

// RenameCommit renames the topmost commit with the given name
func (c *GitCommand) RenameCommit(name string) error {
	return c.OSCommand.RunCommand("git commit --allow-empty --amend -m %s", c.OSCommand.Quote(name))
}

// RebaseBranch interactive rebases onto a branch
func (c *GitCommand) RebaseBranch(branchName string) error {
	cmd, err := c.PrepareInteractiveRebaseCommand(branchName, "", false)
	if err != nil {
		return err
	}

	return c.OSCommand.RunPreparedCommand(cmd)
}

// Fetch fetch git repo
func (c *GitCommand) Fetch(unamePassQuestion func(string) string, canAskForCredentials bool) error {
	return c.OSCommand.DetectUnamePass("git fetch", func(question string) string {
		if canAskForCredentials {
			return unamePassQuestion(question)
		}
		return "\n"
	})
}

// ResetToCommit reset to commit
func (c *GitCommand) ResetToCommit(sha string, strength string) error {
	return c.OSCommand.RunCommand("git reset --%s %s", strength, sha)
}

// NewBranch create new branch
func (c *GitCommand) NewBranch(name string, baseBranch string) error {
	return c.OSCommand.RunCommand("git checkout -b %s %s", name, baseBranch)
}

// CurrentBranchName is a function.
func (c *GitCommand) CurrentBranchName() (string, error) {
	branchName, err := c.OSCommand.RunCommandWithOutput("git symbolic-ref --short HEAD")
	if err != nil || branchName == "HEAD\n" {
		output, err := c.OSCommand.RunCommandWithOutput("git branch --contains")
		if err != nil {
			return "", err
		}
		re := regexp.MustCompile(CurrentBranchNameRegex)
		match := re.FindStringSubmatch(output)
		branchName = match[1]
	}
	return strings.TrimSpace(branchName), nil
}

// DeleteBranch delete branch
func (c *GitCommand) DeleteBranch(branch string, force bool) error {
	command := "git branch -d"

	if force {
		command = "git branch -D"
	}

	return c.OSCommand.RunCommand("%s %s", command, branch)
}

// ListStash list stash
func (c *GitCommand) ListStash() (string, error) {
	return c.OSCommand.RunCommandWithOutput("git stash list")
}

// Merge merge
func (c *GitCommand) Merge(branchName string) error {
	return c.OSCommand.RunCommand("git merge --no-edit %s", branchName)
}

// AbortMerge abort merge
func (c *GitCommand) AbortMerge() error {
	return c.OSCommand.RunCommand("git merge --abort")
}

// usingGpg tells us whether the user has gpg enabled so that we can know
// whether we need to run a subprocess to allow them to enter their password
func (c *GitCommand) usingGpg() bool {
	gpgsign, _ := c.getLocalGitConfig("commit.gpgsign")
	if gpgsign == "" {
		gpgsign, _ = c.getGlobalGitConfig("commit.gpgsign")
	}
	value := strings.ToLower(gpgsign)

	return value == "true" || value == "1" || value == "yes" || value == "on"
}

// Commit commits to git
func (c *GitCommand) Commit(message string, flags string) (*exec.Cmd, error) {
	command := fmt.Sprintf("git commit %s -m %s", flags, c.OSCommand.Quote(message))
	if c.usingGpg() {
		return c.OSCommand.PrepareSubProcess(c.OSCommand.Platform.shell, c.OSCommand.Platform.shellArg, command), nil
	}

	return nil, c.OSCommand.RunCommand(command)
}

// AmendHead amends HEAD with whatever is staged in your working tree
func (c *GitCommand) AmendHead() (*exec.Cmd, error) {
	command := "git commit --amend --no-edit --allow-empty"
	if c.usingGpg() {
		return c.OSCommand.PrepareSubProcess(c.OSCommand.Platform.shell, c.OSCommand.Platform.shellArg, command), nil
	}

	return nil, c.OSCommand.RunCommand(command)
}

// Pull pulls from repo
func (c *GitCommand) Pull(args string, ask func(string) string) error {
	return c.OSCommand.DetectUnamePass("git pull --no-edit "+args, ask)
}

// PullWithoutPasswordCheck assumes that the pull will not prompt the user for a password
func (c *GitCommand) PullWithoutPasswordCheck(args string) error {
	return c.OSCommand.RunCommand("git pull --no-edit " + args)
}

// Push pushes to a branch
func (c *GitCommand) Push(branchName string, force bool, upstream string, args string, ask func(string) string) error {
	forceFlag := ""
	if force {
		forceFlag = "--force-with-lease"
	}

	setUpstreamArg := ""
	if upstream != "" {
		setUpstreamArg = "--set-upstream " + upstream
	}

	cmd := fmt.Sprintf("git push --follow-tags %s %s %s", forceFlag, setUpstreamArg, args)
	return c.OSCommand.DetectUnamePass(cmd, ask)
}

// CatFile obtains the content of a file
func (c *GitCommand) CatFile(fileName string) (string, error) {
	return c.OSCommand.RunCommandWithOutput("cat %s", c.OSCommand.Quote(fileName))
}

// StageFile stages a file
func (c *GitCommand) StageFile(fileName string) error {
	return c.OSCommand.RunCommand("git add %s", c.OSCommand.Quote(fileName))
}

// StageAll stages all files
func (c *GitCommand) StageAll() error {
	return c.OSCommand.RunCommand("git add -A")
}

// UnstageAll stages all files
func (c *GitCommand) UnstageAll() error {
	return c.OSCommand.RunCommand("git reset")
}

// UnStageFile unstages a file
func (c *GitCommand) UnStageFile(fileName string, tracked bool) error {
	command := "git rm --cached %s"
	if tracked {
		command = "git reset HEAD %s"
	}

	// renamed files look like "file1 -> file2"
	fileNames := strings.Split(fileName, " -> ")
	for _, name := range fileNames {
		if err := c.OSCommand.RunCommand(command, c.OSCommand.Quote(name)); err != nil {
			return err
		}
	}
	return nil
}

// GitStatus returns the plaintext short status of the repo
func (c *GitCommand) GitStatus() (string, error) {
	return c.OSCommand.RunCommandWithOutput("git status --untracked-files=all --porcelain")
}

// IsInMergeState states whether we are still mid-merge
func (c *GitCommand) IsInMergeState() (bool, error) {
	output, err := c.OSCommand.RunCommandWithOutput("git status --untracked-files=all")
	if err != nil {
		return false, err
	}
	return strings.Contains(output, "conclude merge") || strings.Contains(output, "unmerged paths"), nil
}

// RebaseMode returns "" for non-rebase mode, "normal" for normal rebase
// and "interactive" for interactive rebase
func (c *GitCommand) RebaseMode() (string, error) {
	exists, err := c.OSCommand.FileExists(fmt.Sprintf("%s/rebase-apply", c.DotGitDir))
	if err != nil {
		return "", err
	}
	if exists {
		return "normal", nil
	}
	exists, err = c.OSCommand.FileExists(fmt.Sprintf("%s/rebase-merge", c.DotGitDir))
	if exists {
		return "interactive", err
	} else {
		return "", err
	}
}

// DiscardAllFileChanges directly
func (c *GitCommand) DiscardAllFileChanges(file *File) error {
	// if the file isn't tracked, we assume you want to delete it
	quotedFileName := c.OSCommand.Quote(file.Name)
	if file.HasStagedChanges || file.HasMergeConflicts {
		if err := c.OSCommand.RunCommand("git reset -- %s", quotedFileName); err != nil {
			return err
		}
	}

	if !file.Tracked {
		return c.removeFile(file.Name)
	}
	return c.DiscardUnstagedFileChanges(file)
}

// DiscardUnstagedFileChanges directly
func (c *GitCommand) DiscardUnstagedFileChanges(file *File) error {
	quotedFileName := c.OSCommand.Quote(file.Name)
	return c.OSCommand.RunCommand("git checkout -- %s", quotedFileName)
}

// Checkout checks out a branch (or commit), with --force if you set the force arg to true
func (c *GitCommand) Checkout(branch string, force bool) error {
	forceArg := ""
	if force {
		forceArg = "--force "
	}
	return c.OSCommand.RunCommand("git checkout %s %s", forceArg, branch)
}

// PrepareCommitSubProcess prepares a subprocess for `git commit`
func (c *GitCommand) PrepareCommitSubProcess() *exec.Cmd {
	return c.OSCommand.PrepareSubProcess("git", "commit")
}

// PrepareCommitAmendSubProcess prepares a subprocess for `git commit --amend --allow-empty`
func (c *GitCommand) PrepareCommitAmendSubProcess() *exec.Cmd {
	return c.OSCommand.PrepareSubProcess("git", "commit", "--amend", "--allow-empty")
}

// GetBranchGraph gets the color-formatted graph of the log for the given branch
// Currently it limits the result to 100 commits, but when we get async stuff
// working we can do lazy loading
func (c *GitCommand) GetBranchGraph(branchName string) (string, error) {
	cmdStr := c.GetBranchGraphCmdStr(branchName)
	return c.OSCommand.RunCommandWithOutput(cmdStr)
}

func (c *GitCommand) GetUpstreamForBranch(branchName string) (string, error) {
	output, err := c.OSCommand.RunCommandWithOutput("git rev-parse --abbrev-ref --symbolic-full-name %s@{u}", branchName)
	return strings.TrimSpace(output), err
}

// Ignore adds a file to the gitignore for the repo
func (c *GitCommand) Ignore(filename string) error {
	return c.OSCommand.AppendLineToFile(".gitignore", filename)
}

func (c *GitCommand) ShowCmdStr(sha string) string {
	return fmt.Sprintf("git show --color=%s --no-renames --stat -p %s", c.colorArg(), sha)
}

func (c *GitCommand) GetBranchGraphCmdStr(branchName string) string {
	return fmt.Sprintf("git log --graph --color=always --abbrev-commit --decorate --date=relative --pretty=medium %s --", branchName)
}

// GetRemoteURL returns current repo remote url
func (c *GitCommand) GetRemoteURL() string {
	url, _ := c.OSCommand.RunCommandWithOutput("git config --get remote.origin.url")
	return utils.TrimTrailingNewline(url)
}

// CheckRemoteBranchExists Returns remote branch
func (c *GitCommand) CheckRemoteBranchExists(branch *Branch) bool {
	_, err := c.OSCommand.RunCommandWithOutput(
		"git show-ref --verify -- refs/remotes/origin/%s",
		branch.Name,
	)

	return err == nil
}

// Diff returns the diff of a file
func (c *GitCommand) Diff(file *File, plain bool, cached bool) string {
	// for now we assume an error means the file was deleted
	s, _ := c.OSCommand.RunCommandWithOutput(c.DiffCmdStr(file, plain, cached))
	return s
}

func (c *GitCommand) DiffCmdStr(file *File, plain bool, cached bool) string {
	cachedArg := ""
	trackedArg := "--"
	colorArg := c.colorArg()
	split := strings.Split(file.Name, " -> ") // in case of a renamed file we get the new filename
	fileName := c.OSCommand.Quote(split[len(split)-1])
	if cached {
		cachedArg = "--cached"
	}
	if !file.Tracked && !file.HasStagedChanges && !cached {
		trackedArg = "--no-index /dev/null"
	}
	if plain {
		colorArg = "never"
	}

	return fmt.Sprintf("git diff --color=%s %s %s %s", colorArg, cachedArg, trackedArg, fileName)
}

func (c *GitCommand) ApplyPatch(patch string, flags ...string) error {
	c.Log.Warn(patch)
	filepath := filepath.Join(c.Config.GetUserConfigDir(), utils.GetCurrentRepoName(), time.Now().Format("Jan _2 15.04.05.000000000")+".patch")
	if err := c.OSCommand.CreateFileWithContent(filepath, patch); err != nil {
		return err
	}

	flagStr := ""
	for _, flag := range flags {
		flagStr += " --" + flag
	}

	return c.OSCommand.RunCommand("git apply %s %s", flagStr, c.OSCommand.Quote(filepath))
}

func (c *GitCommand) FastForward(branchName string, remoteName string, remoteBranchName string) error {
	return c.OSCommand.RunCommand("git fetch %s %s:%s", remoteName, remoteBranchName, branchName)
}

func (c *GitCommand) RunSkipEditorCommand(command string) error {
	cmd := c.OSCommand.ExecutableFromString(command)
	lazyGitPath := c.OSCommand.GetLazygitPath()
	cmd.Env = append(
		cmd.Env,
		"LAZYGIT_CLIENT_COMMAND=EXIT_IMMEDIATELY",
		"EDITOR="+lazyGitPath,
		"VISUAL="+lazyGitPath,
	)
	return c.OSCommand.RunExecutable(cmd)
}

// GenericMerge takes a commandType of "merge" or "rebase" and a command of "abort", "skip" or "continue"
// By default we skip the editor in the case where a commit will be made
func (c *GitCommand) GenericMerge(commandType string, command string) error {
	err := c.RunSkipEditorCommand(
		fmt.Sprintf(
			"git %s --%s",
			commandType,
			command,
		),
	)
	if err != nil {
		return err
	}

	// sometimes we need to do a sequence of things in a rebase but the user needs to
	// fix merge conflicts along the way. When this happens we queue up the next step
	// so that after the next successful rebase continue we can continue from where we left off
	if commandType == "rebase" && command == "continue" && c.onSuccessfulContinue != nil {
		f := c.onSuccessfulContinue
		c.onSuccessfulContinue = nil
		return f()
	}
	if command == "abort" {
		c.onSuccessfulContinue = nil
	}
	return nil
}

func (c *GitCommand) RewordCommit(commits []*Commit, index int) (*exec.Cmd, error) {
	todo, sha, err := c.GenerateGenericRebaseTodo(commits, index, "reword")
	if err != nil {
		return nil, err
	}

	return c.PrepareInteractiveRebaseCommand(sha, todo, false)
}

func (c *GitCommand) MoveCommitDown(commits []*Commit, index int) error {
	// we must ensure that we have at least two commits after the selected one
	if len(commits) <= index+2 {
		// assuming they aren't picking the bottom commit
		return errors.New(c.Tr.SLocalize("NoRoom"))
	}

	todo := ""
	orderedCommits := append(commits[0:index], commits[index+1], commits[index])
	for _, commit := range orderedCommits {
		todo = "pick " + commit.Sha + " " + commit.Name + "\n" + todo
	}

	cmd, err := c.PrepareInteractiveRebaseCommand(commits[index+2].Sha, todo, true)
	if err != nil {
		return err
	}

	return c.OSCommand.RunPreparedCommand(cmd)
}

func (c *GitCommand) InteractiveRebase(commits []*Commit, index int, action string) error {
	todo, sha, err := c.GenerateGenericRebaseTodo(commits, index, action)
	if err != nil {
		return err
	}

	cmd, err := c.PrepareInteractiveRebaseCommand(sha, todo, true)
	if err != nil {
		return err
	}

	return c.OSCommand.RunPreparedCommand(cmd)
}

// PrepareInteractiveRebaseCommand returns the cmd for an interactive rebase
// we tell git to run lazygit to edit the todo list, and we pass the client
// lazygit a todo string to write to the todo file
func (c *GitCommand) PrepareInteractiveRebaseCommand(baseSha string, todo string, overrideEditor bool) (*exec.Cmd, error) {
	ex := c.OSCommand.GetLazygitPath()

	debug := "FALSE"
	if c.OSCommand.Config.GetDebug() {
		debug = "TRUE"
	}

	splitCmd := str.ToArgv(fmt.Sprintf("git rebase --interactive --autostash --keep-empty --rebase-merges %s", baseSha))

	cmd := c.OSCommand.command(splitCmd[0], splitCmd[1:]...)

	gitSequenceEditor := ex
	if todo == "" {
		gitSequenceEditor = "true"
	}

	cmd.Env = os.Environ()
	cmd.Env = append(
		cmd.Env,
		"LAZYGIT_CLIENT_COMMAND=INTERACTIVE_REBASE",
		"LAZYGIT_REBASE_TODO="+todo,
		"DEBUG="+debug,
		"LANG=en_US.UTF-8",   // Force using EN as language
		"LC_ALL=en_US.UTF-8", // Force using EN as language
		"GIT_SEQUENCE_EDITOR="+gitSequenceEditor,
	)

	if overrideEditor {
		cmd.Env = append(cmd.Env, "EDITOR="+ex)
	}

	return cmd, nil
}

func (c *GitCommand) HardReset(baseSha string) error {
	return c.OSCommand.RunCommand("git reset --hard " + baseSha)
}

func (c *GitCommand) SoftReset(baseSha string) error {
	return c.OSCommand.RunCommand("git reset --soft " + baseSha)
}

func (c *GitCommand) GenerateGenericRebaseTodo(commits []*Commit, actionIndex int, action string) (string, string, error) {
	baseIndex := actionIndex + 1

	if len(commits) <= baseIndex {
		return "", "", errors.New(c.Tr.SLocalize("CannotRebaseOntoFirstCommit"))
	}

	if action == "squash" || action == "fixup" {
		baseIndex++

		if len(commits) <= baseIndex {
			return "", "", errors.New(c.Tr.SLocalize("CannotSquashOntoSecondCommit"))
		}
	}

	todo := ""
	for i, commit := range commits[0:baseIndex] {
		a := "pick"
		if i == actionIndex {
			a = action
		}
		todo = a + " " + commit.Sha + " " + commit.Name + "\n" + todo
	}

	return todo, commits[baseIndex].Sha, nil
}

// AmendTo amends the given commit with whatever files are staged
func (c *GitCommand) AmendTo(sha string) error {
	if err := c.CreateFixupCommit(sha); err != nil {
		return err
	}

	return c.SquashAllAboveFixupCommits(sha)
}

// EditRebaseTodo sets the action at a given index in the git-rebase-todo file
func (c *GitCommand) EditRebaseTodo(index int, action string) error {
	fileName := fmt.Sprintf("%s/rebase-merge/git-rebase-todo", c.DotGitDir)
	bytes, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}

	content := strings.Split(string(bytes), "\n")
	commitCount := c.getTodoCommitCount(content)

	// we have the most recent commit at the bottom whereas the todo file has
	// it at the bottom, so we need to subtract our index from the commit count
	contentIndex := commitCount - 1 - index
	splitLine := strings.Split(content[contentIndex], " ")
	content[contentIndex] = action + " " + strings.Join(splitLine[1:], " ")
	result := strings.Join(content, "\n")

	return ioutil.WriteFile(fileName, []byte(result), 0644)
}

func (c *GitCommand) getTodoCommitCount(content []string) int {
	// count lines that are not blank and are not comments
	commitCount := 0
	for _, line := range content {
		if line != "" && !strings.HasPrefix(line, "#") {
			commitCount++
		}
	}
	return commitCount
}

// MoveTodoDown moves a rebase todo item down by one position
func (c *GitCommand) MoveTodoDown(index int) error {
	fileName := fmt.Sprintf("%s/rebase-merge/git-rebase-todo", c.DotGitDir)
	bytes, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}

	content := strings.Split(string(bytes), "\n")
	commitCount := c.getTodoCommitCount(content)
	contentIndex := commitCount - 1 - index

	rearrangedContent := append(content[0:contentIndex-1], content[contentIndex], content[contentIndex-1])
	rearrangedContent = append(rearrangedContent, content[contentIndex+1:]...)
	result := strings.Join(rearrangedContent, "\n")

	return ioutil.WriteFile(fileName, []byte(result), 0644)
}

// Revert reverts the selected commit by sha
func (c *GitCommand) Revert(sha string) error {
	return c.OSCommand.RunCommand("git revert %s", sha)
}

// CherryPickCommits begins an interactive rebase with the given shas being cherry picked onto HEAD
func (c *GitCommand) CherryPickCommits(commits []*Commit) error {
	todo := ""
	for _, commit := range commits {
		todo = "pick " + commit.Sha + " " + commit.Name + "\n" + todo
	}

	cmd, err := c.PrepareInteractiveRebaseCommand("HEAD", todo, false)
	if err != nil {
		return err
	}

	return c.OSCommand.RunPreparedCommand(cmd)
}

// GetCommitFiles get the specified commit files
func (c *GitCommand) GetCommitFiles(commitSha string, patchManager *PatchManager) ([]*CommitFile, error) {
	files, err := c.OSCommand.RunCommandWithOutput("git show --pretty= --name-only --no-renames %s", commitSha)
	if err != nil {
		return nil, err
	}

	commitFiles := make([]*CommitFile, 0)

	for _, file := range strings.Split(strings.TrimRight(files, "\n"), "\n") {
		status := UNSELECTED
		if patchManager != nil && patchManager.CommitSha == commitSha {
			status = patchManager.GetFileStatus(file)
		}

		commitFiles = append(commitFiles, &CommitFile{
			Sha:           commitSha,
			Name:          file,
			DisplayString: file,
			Status:        status,
		})
	}

	return commitFiles, nil
}

// ShowCommitFile get the diff of specified commit file
func (c *GitCommand) ShowCommitFile(commitSha, fileName string, plain bool) (string, error) {
	cmdStr := c.ShowCommitFileCmdStr(commitSha, fileName, plain)
	return c.OSCommand.RunCommandWithOutput(cmdStr)
}

func (c *GitCommand) ShowCommitFileCmdStr(commitSha, fileName string, plain bool) string {
	colorArg := c.colorArg()
	if plain {
		colorArg = "never"
	}

	return fmt.Sprintf("git show --no-renames --color=%s %s -- %s", colorArg, commitSha, fileName)
}

// CheckoutFile checks out the file for the given commit
func (c *GitCommand) CheckoutFile(commitSha, fileName string) error {
	return c.OSCommand.RunCommand("git checkout %s %s", commitSha, fileName)
}

// DiscardOldFileChanges discards changes to a file from an old commit
func (c *GitCommand) DiscardOldFileChanges(commits []*Commit, commitIndex int, fileName string) error {
	if err := c.BeginInteractiveRebaseForCommit(commits, commitIndex); err != nil {
		return err
	}

	// check if file exists in previous commit (this command returns an error if the file doesn't exist)
	if err := c.OSCommand.RunCommand("git cat-file -e HEAD^:%s", fileName); err != nil {
		if err := c.OSCommand.Remove(fileName); err != nil {
			return err
		}
		if err := c.StageFile(fileName); err != nil {
			return err
		}
	} else if err := c.CheckoutFile("HEAD^", fileName); err != nil {
		return err
	}

	// amend the commit
	cmd, err := c.AmendHead()
	if cmd != nil {
		return errors.New("received unexpected pointer to cmd")
	}
	if err != nil {
		return err
	}

	// continue
	return c.GenericMerge("rebase", "continue")
}

// DiscardAnyUnstagedFileChanges discards any unstages file changes via `git checkout -- .`
func (c *GitCommand) DiscardAnyUnstagedFileChanges() error {
	return c.OSCommand.RunCommand("git checkout -- .")
}

// RemoveTrackedFiles will delete the given file(s) even if they are currently tracked
func (c *GitCommand) RemoveTrackedFiles(name string) error {
	return c.OSCommand.RunCommand("git rm -r --cached %s", name)
}

// RemoveUntrackedFiles runs `git clean -fd`
func (c *GitCommand) RemoveUntrackedFiles() error {
	return c.OSCommand.RunCommand("git clean -fd")
}

// ResetHardHead runs `git reset --hard`
func (c *GitCommand) ResetHard(ref string) error {
	return c.OSCommand.RunCommand("git reset --hard " + ref)
}

// ResetSoft runs `git reset --soft HEAD`
func (c *GitCommand) ResetSoft(ref string) error {
	return c.OSCommand.RunCommand("git reset --soft " + ref)
}

// DiffCommits show diff between commits
func (c *GitCommand) DiffCommits(sha1, sha2 string) (string, error) {
	return c.OSCommand.RunCommandWithOutput("git diff --color=%s --stat -p %s %s", c.colorArg(), sha1, sha2)
}

// CreateFixupCommit creates a commit that fixes up a previous commit
func (c *GitCommand) CreateFixupCommit(sha string) error {
	return c.OSCommand.RunCommand("git commit --fixup=%s", sha)
}

// SquashAllAboveFixupCommits squashes all fixup! commits above the given one
func (c *GitCommand) SquashAllAboveFixupCommits(sha string) error {
	return c.RunSkipEditorCommand(
		fmt.Sprintf(
			"git rebase --interactive --autostash --autosquash %s^",
			sha,
		),
	)
}

// StashSaveStagedChanges stashes only the currently staged changes. This takes a few steps
// shoutouts to Joe on https://stackoverflow.com/questions/14759748/stashing-only-staged-changes-in-git-is-it-possible
func (c *GitCommand) StashSaveStagedChanges(message string) error {

	if err := c.OSCommand.RunCommand("git stash --keep-index"); err != nil {
		return err
	}

	if err := c.StashSave(message); err != nil {
		return err
	}

	if err := c.OSCommand.RunCommand("git stash apply stash@{1}"); err != nil {
		return err
	}

	if err := c.OSCommand.PipeCommands("git stash show -p", "git apply -R"); err != nil {
		return err
	}

	if err := c.OSCommand.RunCommand("git stash drop stash@{1}"); err != nil {
		return err
	}

	// if you had staged an untracked file, that will now appear as 'AD' in git status
	// meaning it's deleted in your working tree but added in your index. Given that it's
	// now safely stashed, we need to remove it.
	files := c.GetStatusFiles()
	for _, file := range files {
		if file.ShortStatus == "AD" {
			if err := c.UnStageFile(file.Name, false); err != nil {
				return err
			}
		}
	}

	return nil
}

// BeginInteractiveRebaseForCommit starts an interactive rebase to edit the current
// commit and pick all others. After this you'll want to call `c.GenericMerge("rebase", "continue")`
func (c *GitCommand) BeginInteractiveRebaseForCommit(commits []*Commit, commitIndex int) error {
	if len(commits)-1 < commitIndex {
		return errors.New("index outside of range of commits")
	}

	// we can make this GPG thing possible it just means we need to do this in two parts:
	// one where we handle the possibility of a credential request, and the other
	// where we continue the rebase
	if c.usingGpg() {
		return errors.New(c.Tr.SLocalize("DisabledForGPG"))
	}

	todo, sha, err := c.GenerateGenericRebaseTodo(commits, commitIndex, "edit")
	if err != nil {
		return err
	}

	cmd, err := c.PrepareInteractiveRebaseCommand(sha, todo, true)
	if err != nil {
		return err
	}

	if err := c.OSCommand.RunPreparedCommand(cmd); err != nil {
		return err
	}

	return nil
}

func (c *GitCommand) SetUpstreamBranch(upstream string) error {
	return c.OSCommand.RunCommand("git branch -u %s", upstream)
}

func (c *GitCommand) AddRemote(name string, url string) error {
	return c.OSCommand.RunCommand("git remote add %s %s", name, url)
}

func (c *GitCommand) RemoveRemote(name string) error {
	return c.OSCommand.RunCommand("git remote remove %s", name)
}

func (c *GitCommand) IsHeadDetached() bool {
	err := c.OSCommand.RunCommand("git symbolic-ref -q HEAD")
	return err != nil
}

func (c *GitCommand) DeleteRemoteBranch(remoteName string, branchName string) error {
	return c.OSCommand.RunCommand("git push %s --delete %s", remoteName, branchName)
}

func (c *GitCommand) SetBranchUpstream(remoteName string, remoteBranchName string, branchName string) error {
	return c.OSCommand.RunCommand("git branch --set-upstream-to=%s/%s %s", remoteName, remoteBranchName, branchName)
}

func (c *GitCommand) RenameRemote(oldRemoteName string, newRemoteName string) error {
	return c.OSCommand.RunCommand("git remote rename %s %s", oldRemoteName, newRemoteName)
}

func (c *GitCommand) UpdateRemoteUrl(remoteName string, updatedUrl string) error {
	return c.OSCommand.RunCommand("git remote set-url %s %s", remoteName, updatedUrl)
}

func (c *GitCommand) CreateLightweightTag(tagName string, commitSha string) error {
	return c.OSCommand.RunCommand("git tag %s %s", tagName, commitSha)
}

func (c *GitCommand) DeleteTag(tagName string) error {
	return c.OSCommand.RunCommand("git tag -d %s", tagName)
}

func (c *GitCommand) PushTag(remoteName string, tagName string) error {
	return c.OSCommand.RunCommand("git push %s %s", remoteName, tagName)
}

func (c *GitCommand) FetchRemote(remoteName string) error {
	return c.OSCommand.RunCommand("git fetch %s", remoteName)
}

func (c *GitCommand) GetReflogCommits() ([]*Commit, error) {
	output, err := c.OSCommand.RunCommandWithOutput("git reflog --abbrev=20")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(output), "\n")
	commits := make([]*Commit, len(lines))
	re := regexp.MustCompile(`(\w+).*HEAD@\{\d+\}: (.*)`)
	for i, line := range lines {
		match := re.FindStringSubmatch(line)
		if len(match) <= 1 {
			continue
		}

		commits[i] = &Commit{
			Sha:    match[1],
			Name:   match[2],
			Status: "reflog",
		}
	}

	return commits, nil
}

func (c *GitCommand) ConfiguredPager() string {
	if os.Getenv("GIT_PAGER") != "" {
		return os.Getenv("GIT_PAGER")
	}
	if os.Getenv("PAGER") != "" {
		return os.Getenv("PAGER")
	}
	output, err := c.OSCommand.RunCommandWithOutput("git config --get-all core.pager")
	if err != nil {
		return ""
	}
	trimmedOutput := strings.TrimSpace(output)
	return strings.Split(trimmedOutput, "\n")[0]
}

func (c *GitCommand) GetPager(width int) string {
	useConfig := c.Config.GetUserConfig().GetBool("git.paging.useConfig")
	if useConfig {
		pager := c.ConfiguredPager()
		return strings.Split(pager, "| less")[0]
	}

	templateValues := map[string]string{
		"columnWidth": strconv.Itoa(width/2 - 6),
	}

	pagerTemplate := c.Config.GetUserConfig().GetString("git.paging.pager")
	return utils.ResolvePlaceholderString(pagerTemplate, templateValues)
}

func (c *GitCommand) colorArg() string {
	return c.Config.GetUserConfig().GetString("git.paging.colorArg")
}

func (c *GitCommand) RenameBranch(oldName string, newName string) error {
	return c.OSCommand.RunCommand("git branch --move %s %s", oldName, newName)
}
