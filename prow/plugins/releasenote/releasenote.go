/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package releasenote

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/plugins"
)

const pluginName = "release-note"

const (
	// deprecatedReleaseNoteLabelNeeded is the previous version of the
	// releaseNotLabelNeeded label, which we continue to honor for the
	// time being
	deprecatedReleaseNoteLabelNeeded = "release-note-label-needed"

	releaseNoteLabelNeeded    = "do-not-merge/release-note-label-needed"
	releaseNote               = "release-note"
	releaseNoteNone           = "release-note-none"
	releaseNoteActionRequired = "release-note-action-required"

	releaseNoteFormat       = `Adding %s because the release note process has not been followed.`
	releaseNoteSuffixFormat = `One of the following labels is required %q, %q, or %q.
Please see: https://github.com/kubernetes/community/blob/master/contributors/devel/pull-requests.md#write-release-notes-if-needed.`
	parentReleaseNoteFormat = `All 'parent' PRs of a cherry-pick PR must have one of the %q or %q labels, or this PR must follow the standard/parent release note labeling requirement.`

	noReleaseNoteComment = "none"
	actionRequiredNote   = "action required"
)

var (
	releaseNoteSuffix         = fmt.Sprintf(releaseNoteSuffixFormat, releaseNote, releaseNoteActionRequired, releaseNoteNone)
	releaseNoteBody           = fmt.Sprintf(releaseNoteFormat, releaseNoteLabelNeeded)
	deprecatedReleaseNoteBody = fmt.Sprintf(releaseNoteFormat, deprecatedReleaseNoteLabelNeeded)
	parentReleaseNoteBody     = fmt.Sprintf(parentReleaseNoteFormat, releaseNote, releaseNoteActionRequired)

	noteMatcherRE = regexp.MustCompile(`(?s)(?:Release note\*\*:\s*(?:<!--[^<>]*-->\s*)?` + "```(?:release-note)?|```release-note)(.+?)```")
	cpRe          = regexp.MustCompile(`Cherry pick of #([[:digit:]]+) on release-([[:digit:]]+\.[[:digit:]]+).`)

	allRNLabels = []string{
		releaseNoteNone,
		releaseNoteActionRequired,
		deprecatedReleaseNoteLabelNeeded,
		releaseNoteLabelNeeded,
		releaseNote,
	}

	releaseNoteRe               = regexp.MustCompile(`(?mi)^/release-note\s*$`)
	releaseNoteNoneRe           = regexp.MustCompile(`(?mi)^/release-note-none\s*$`)
	releaseNoteActionRequiredRe = regexp.MustCompile(`(?mi)^/release-note-action-required\s*$`)
)

func init() {
	plugins.RegisterIssueCommentHandler(pluginName, handleIssueComment)
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest)
}

type githubClient interface {
	IsMember(org, user string) (bool, error)
	CreateComment(owner, repo string, number int, comment string) error
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	DeleteStaleComments(org, repo string, number int, comments []github.IssueComment, isStale func(github.IssueComment) bool) error
	BotName() (string, error)
}

func handleIssueComment(pc plugins.PluginClient, ic github.IssueCommentEvent) error {
	return handleComment(pc.GitHubClient, pc.Logger, ic)
}

func handleComment(gc githubClient, log *logrus.Entry, ic github.IssueCommentEvent) error {
	// Only consider PRs and new comments.
	if !ic.Issue.IsPullRequest() || ic.Action != github.IssueCommentActionCreated {
		return nil
	}

	org := ic.Repo.Owner.Login
	repo := ic.Repo.Name
	number := ic.Issue.Number

	// Which label does the comment want us to add?
	var nl string
	switch {
	case releaseNoteRe.MatchString(ic.Comment.Body):
		nl = releaseNote
	case releaseNoteNoneRe.MatchString(ic.Comment.Body):
		nl = releaseNoteNone
	case releaseNoteActionRequiredRe.MatchString(ic.Comment.Body):
		nl = releaseNoteActionRequired
	default:
		return nil
	}

	// Emit deprecation warning for /release-note and /release-note-action-required.
	if nl == releaseNote || nl == releaseNoteActionRequired {
		format := "the `/%s` and `/%s` commands have been deprecated.\nPlease edit the `release-note` block in the PR body text to include the release note. If the release note requires additional action include the string `action required` in the release note. For example:\n````\n```release-note\nSome release note with action required.\n```\n````"
		resp := fmt.Sprintf(format, releaseNote, releaseNoteActionRequired)
		return gc.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, resp))
	}

	// Only allow authors and org members to add labels.
	isMember, err := gc.IsMember(ic.Repo.Owner.Login, ic.Comment.User.Login)
	if err != nil {
		return err
	}

	isAuthor := ic.Issue.IsAuthor(ic.Comment.User.Login)

	if !isMember && !isAuthor {
		format := "you can only set the release note label to %s if you are the PR author or an org member."
		resp := fmt.Sprintf(format, releaseNoteNone)
		return gc.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, resp))
	}

	// Don't allow the /release-note-none command if the release-note block contains a valid release note.
	blockNL := determineReleaseNoteLabel(ic.Issue.Body)
	if blockNL == releaseNote || blockNL == releaseNoteActionRequired {
		format := "you can only set the release note label to %s if the release-note block in the PR body text is empty or \"none\"."
		resp := fmt.Sprintf(format, releaseNoteNone)
		return gc.CreateComment(org, repo, number, plugins.FormatICResponse(ic.Comment, resp))
	}
	if !ic.Issue.HasLabel(releaseNoteNone) {
		if err := gc.AddLabel(org, repo, number, releaseNoteNone); err != nil {
			return err
		}
	}
	// Remove all other release-note-* labels if necessary.
	return removeOtherLabels(
		func(l string) error {
			return gc.RemoveLabel(org, repo, number, l)
		},
		releaseNoteNone,
		allRNLabels,
		ic.Issue.Labels,
	)
}

func removeOtherLabels(remover func(string) error, label string, labelSet []string, currentLabels []github.Label) error {
	var errs []error
	for _, elem := range labelSet {
		if elem != label && hasLabel(elem, currentLabels) {
			if err := remover(elem); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("encountered %d errors setting labels: %v", len(errs), errs)
	}
	return nil
}

func handlePullRequest(pc plugins.PluginClient, pr github.PullRequestEvent) error {
	return handlePR(pc.GitHubClient, pc.Logger, &pr)
}

func handlePR(gc githubClient, log *logrus.Entry, pr *github.PullRequestEvent) error {
	// Only consider events that edit the PR body.
	if pr.Action != github.PullRequestActionOpened && pr.Action != github.PullRequestActionEdited {
		return nil
	}
	org := pr.Repo.Owner.Login
	repo := pr.Repo.Name

	prLabels, err := gc.GetIssueLabels(org, repo, pr.Number)
	if err != nil {
		return fmt.Errorf("failed to list labels on PR #%d. err: %v", pr.Number, err)
	}

	var comments []github.IssueComment
	labelToAdd := determineReleaseNoteLabel(pr.PullRequest.Body)
	if labelToAdd == releaseNoteLabelNeeded {
		if !prMustFollowRelNoteProcess(gc, log, pr, prLabels, true) {
			ensureNoRelNoteNeededLabel(gc, log, pr, prLabels)
			return clearStaleComments(gc, log, pr, prLabels, nil)
		}
		// If /release-note-none has been left on PR then pretend the release-note body is "NONE" instead of empty.
		comments, err = gc.ListIssueComments(org, repo, pr.Number)
		if err != nil {
			return fmt.Errorf("failed to list comments on %s/%s#%d. err: %v", org, repo, pr.Number, err)
		}
		if containsNoneCommand(comments) {
			labelToAdd = releaseNoteNone
		}
	}
	if labelToAdd == releaseNoteLabelNeeded {
		if !hasLabel(releaseNoteLabelNeeded, prLabels) {
			comment := plugins.FormatResponse(pr.PullRequest.User.Login, releaseNoteBody, releaseNoteSuffix)
			if err := gc.CreateComment(org, repo, pr.Number, comment); err != nil {
				log.WithError(err).Errorf("Failed to comment on %s/%s#%d with comment %q.", org, repo, pr.Number, comment)
			}
		}
	} else {
		//going to apply some other release-note-label
		ensureNoRelNoteNeededLabel(gc, log, pr, prLabels)
	}

	// Add the label if needed
	if !hasLabel(labelToAdd, prLabels) {
		if err = gc.AddLabel(org, repo, pr.Number, labelToAdd); err != nil {
			return err
		}
	}

	err = removeOtherLabels(
		func(l string) error {
			return gc.RemoveLabel(org, repo, pr.Number, l)
		},
		labelToAdd,
		allRNLabels,
		prLabels,
	)
	if err != nil {
		log.Error(err)
	}

	return clearStaleComments(gc, log, pr, prLabels, comments)
}

func clearStaleComments(gc githubClient, log *logrus.Entry, pr *github.PullRequestEvent, prLabels []github.Label, comments []github.IssueComment) error {
	// Clean up old comments.
	// If the PR must follow the process and hasn't yet completed the process, don't remove comments.
	if prMustFollowRelNoteProcess(gc, log, pr, prLabels, false) && !releaseNoteAlreadyAdded(prLabels) {
		return nil
	}
	botName, err := gc.BotName()
	if err != nil {
		return err
	}
	return gc.DeleteStaleComments(
		pr.Repo.Owner.Login,
		pr.Repo.Name,
		pr.Number,
		comments,
		func(c github.IssueComment) bool { // isStale function
			return c.User.Login == botName &&
				(strings.Contains(c.Body, releaseNoteBody) ||
					strings.Contains(c.Body, parentReleaseNoteBody) ||
					strings.Contains(c.Body, deprecatedReleaseNoteBody))
		},
	)
}

func containsNoneCommand(comments []github.IssueComment) bool {
	for _, c := range comments {
		if releaseNoteNoneRe.MatchString(c.Body) {
			return true
		}
	}
	return false
}

func ensureNoRelNoteNeededLabel(gc githubClient, log *logrus.Entry, pr *github.PullRequestEvent, prLabels []github.Label) {
	org := pr.Repo.Owner.Login
	repo := pr.Repo.Name
	format := "Failed to remove the label %q from %s/%s#%d."
	if hasLabel(releaseNoteLabelNeeded, prLabels) {
		if err := gc.RemoveLabel(org, repo, pr.Number, releaseNoteLabelNeeded); err != nil {
			log.WithError(err).Errorf(format, releaseNoteLabelNeeded, org, repo, pr.Number)
		}
	}
	if hasLabel(deprecatedReleaseNoteLabelNeeded, prLabels) {
		if err := gc.RemoveLabel(org, repo, pr.Number, deprecatedReleaseNoteLabelNeeded); err != nil {
			log.WithError(err).Errorf(format, deprecatedReleaseNoteLabelNeeded, org, repo, pr.Number)
		}
	}
}

// determineReleaseNoteLabel returns the label to be added based on the contents of the 'release-note'
// section of a PR's body text.
func determineReleaseNoteLabel(body string) string {
	composedReleaseNote := strings.ToLower(strings.TrimSpace(getReleaseNote(body)))

	if composedReleaseNote == "" {
		return releaseNoteLabelNeeded
	}
	if composedReleaseNote == noReleaseNoteComment {
		return releaseNoteNone
	}
	if strings.Contains(composedReleaseNote, actionRequiredNote) {
		return releaseNoteActionRequired
	}
	return releaseNote
}

// getReleaseNote returns the release note from a PR body
// assumes that the PR body followed the PR template
func getReleaseNote(body string) string {
	potentialMatch := noteMatcherRE.FindStringSubmatch(body)
	if potentialMatch == nil {
		return ""
	}
	return strings.TrimSpace(potentialMatch[1])
}

func releaseNoteAlreadyAdded(prLabels []github.Label) bool {
	return hasLabel(releaseNote, prLabels) ||
		hasLabel(releaseNoteActionRequired, prLabels) ||
		hasLabel(releaseNoteNone, prLabels)
}

func prMustFollowRelNoteProcess(gc githubClient, log *logrus.Entry, pr *github.PullRequestEvent, prLabels []github.Label, comment bool) bool {
	if pr.PullRequest.Base.Ref == "master" {
		return true
	}

	parents := getCherrypickParentPRNums(pr.PullRequest.Body)
	// if it has no parents it needs to follow the release note process
	if len(parents) == 0 {
		return true
	}

	org := pr.Repo.Owner.Login
	repo := pr.Repo.Name

	var notelessParents []string
	for _, parent := range parents {
		// If the parent didn't set a release note, the CP must
		parentLabels, err := gc.GetIssueLabels(org, repo, parent)
		if err != nil {
			log.WithError(err).Errorf("Failed to list labels on PR #%d (parent of #%d).", parent, pr.Number)
			continue
		}
		if !hasLabel(releaseNote, parentLabels) &&
			!hasLabel(releaseNoteActionRequired, parentLabels) {
			notelessParents = append(notelessParents, "#"+strconv.Itoa(parent))
		}
	}
	if len(notelessParents) == 0 {
		// All of the parents set the releaseNote or releaseNoteActionRequired label,
		// so this cherrypick PR needs to do nothing.
		return false
	}

	if comment && !hasLabel(releaseNoteLabelNeeded, prLabels) {
		comment := plugins.FormatResponse(
			pr.PullRequest.User.Login,
			parentReleaseNoteBody,
			fmt.Sprintf("The following parent PRs have neither the %q nor the %q labels: %s.",
				releaseNote,
				releaseNoteActionRequired,
				strings.Join(notelessParents, ", "),
			),
		)
		if err := gc.CreateComment(org, repo, pr.Number, comment); err != nil {
			log.WithError(err).Errorf("Error creating comment on %s/%s#%d with comment %q.", org, repo, pr.Number, comment)
		}
	}
	return true
}

func getCherrypickParentPRNums(body string) []int {
	lines := strings.Split(body, "\n")

	var out []int
	for _, line := range lines {
		matches := cpRe.FindStringSubmatch(line)
		if len(matches) != 3 {
			continue
		}
		parentNum, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}
		out = append(out, parentNum)
	}
	return out
}

func hasLabel(label string, issueLabels []github.Label) bool {
	label = strings.ToLower(label)
	for _, l := range issueLabels {
		if strings.ToLower(l.Name) == label {
			return true
		}
	}
	return false
}
