package environments

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/cenkalti/backoff"

	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/cli"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/giturl"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
)

// Git lazily create a gitter if its not specified
func (o *EnvironmentPullRequestOptions) Git() gitclient.Interface {
	if o.Gitter == nil {
		if o.CommandRunner == nil {
			// lets use a quiet runner to avoid outputting the user/token on git clones
			o.CommandRunner = cmdrunner.QuietCommandRunner
		}
		o.Gitter = cli.NewCLIClient("", o.CommandRunner)
	}
	return o.Gitter
}

// CreatePullRequest crates a pull request if there are git changes
func (o *EnvironmentPullRequestOptions) CreatePullRequest(scmClient *scm.Client, gitURL, repoFullName, dir string, doneCommit bool, existingPR *scm.PullRequest) (*scm.PullRequest, error) {
	gitter := o.Git()
	changes, err := gitclient.HasChanges(gitter, dir)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to detect if there were git changes in dir %s", dir)
	}
	if !changes && !doneCommit {
		log.Logger().Infof("no changes detected so not creating a Pull Request on %s", termcolor.ColorInfo(gitURL))
		return nil, nil
	}

	const headBranchPrefix = "HEAD branch:"
	baseBranch := o.BaseBranchName
	if baseBranch == "" {
		if o.RemoteName == "" {
			o.RemoteName = "origin"
		}
		text, err := gitter.Command(dir, "rev-parse", "--abbrev-ref", o.RemoteName+"/HEAD")
		if err != nil {
			text, err = gitter.Command(dir, "remote", "show", o.RemoteName)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to get the remote branch name for %s", o.RemoteName)
			}

			lines := strings.Split(text, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, headBranchPrefix) {
					baseBranch = strings.TrimSpace(strings.TrimPrefix(line, headBranchPrefix))
					if baseBranch != "" {
						break
					}
				}
			}
			if baseBranch == "" {
				return nil, errors.Errorf("output of git remote show %s has no prefix %s as was: %s", o.RemoteName, headBranchPrefix, text)
			}
		} else {
			text = strings.TrimSpace(text)
			text = strings.TrimPrefix(text, o.RemoteName)
			baseBranch = strings.TrimPrefix(text, "/")
		}
	}
	if baseBranch == "" {
		baseBranch, err = gitclient.Branch(gitter, dir)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find branch in dir %s", dir)
		}
	}
	if existingPR != nil {
		log.Logger().Debugf("adding to Pull Request %s on branch %s", existingPR.Link, baseBranch)
	} else {
		log.Logger().Debugf("creating Pull Request from %s branch", baseBranch)
	}

	if o.BranchName == "" {
		o.BranchName, err = gitclient.CreateBranch(gitter, dir)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create git branch in %s", dir)
		}
	}

	commitTitle := strings.TrimSpace(o.CommitTitle)
	commitBody := o.CommitMessage
	if commitBody == "" {
		commitBody = o.CommitMessageSuffix
	} else {
		commitBody += "\n" + o.CommitMessageSuffix
	}

	commitMessage := fmt.Sprintf("%s\n\n%s", commitTitle, commitBody)
	_, err = gitclient.AddAndCommitFiles(gitter, dir, strings.TrimSpace(commitMessage))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to commit changes in dir %s", dir)
	}

	err = gitclient.ForcePushBranch(gitter, dir, o.BranchName, o.BranchName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to push to branch %s from dir %s", o.BranchName, dir)
	}

	ctx := context.Background()

	headPrefix := ""
	if o.Fork {
		user, _, err := scmClient.Users.Find(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to find current SCM user")
		}

		username := user.Login
		headPrefix = username + ":"
	}

	head := headPrefix + o.BranchName

	if existingPR != nil {
		return o.addLabelsToPullRequest(ctx, scmClient, repoFullName, existingPR)
	}
	pri := &scm.PullRequestInput{
		Title: commitTitle,
		Head:  head,
		Base:  baseBranch,
		Body:  commitBody,
	}
	pr, _, err := scmClient.PullRequests.Create(ctx, repoFullName, pri)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create PullRequest on %s", gitURL)
	}

	// the URL should not really end in .diff - fix in go-scm
	link := strings.TrimSuffix(pr.Link, ".diff")
	pr.Link = link
	log.Logger().Infof("Created Pull Request: %s", termcolor.ColorInfo(link))

	return o.addLabelsToPullRequest(ctx, scmClient, repoFullName, pr)
}

func (o *EnvironmentPullRequestOptions) GetScmClient(gitURL, kind string) (*scm.Client, string, error) {
	if gitURL == "" {
		log.Logger().Infof("no git URL specified so cannot create a Pull Request")
		return nil, "", nil
	}
	gitInfo, err := giturl.ParseGitURL(gitURL)
	if err != nil {
		return nil, "", errors.Wrapf(err, "failed to parse git URL")
	}

	// coding kind url pattern
	if kind == "coding" {
		u, err := url.Parse(gitURL)
		if err == nil {
			arrs := strings.Split(u.Path, "/")
			if len(arrs) >= 2 {
				gitInfo.Organisation = arrs[len(arrs)-2]
				gitInfo.Project = gitInfo.Organisation
			}
		}

	}

	serverURL := gitInfo.HostURLWithoutUser()
	owner := gitInfo.Organisation

	scmClient, _, err := o.CreateScmClient(serverURL, owner, kind)
	if err != nil {
		return nil, "", errors.Wrapf(err, "failed to create SCM client for %s", gitURL)
	}
	o.ScmClient = scmClient
	repoFullName := scm.Join(gitInfo.Organisation, gitInfo.Name)

	return scmClient, repoFullName, nil
}

func (o *EnvironmentPullRequestOptions) addLabelsToPullRequest(ctx context.Context, scmClient *scm.Client, repoFullName string, pr *scm.PullRequest) (*scm.PullRequest, error) {
	prNumber := pr.Number
	modified := false
	for _, label := range o.Labels {
		if !scmhelpers.ContainsLabel(pr.Labels, label) {
			_, err := scmClient.PullRequests.AddLabel(ctx, repoFullName, prNumber, label)
			if err != nil {
				return pr, errors.Wrapf(err, "failed to add label %s to PR #%d on repo %s", label, prNumber, repoFullName)
			}
			modified = true
		}
	}
	if !modified {
		return pr, nil
	}
	var err error

	// lets use a retry loop to push in case the repository is not yet setup quite yet
	f := func() error {
		pr, _, err = scmClient.PullRequests.Find(ctx, repoFullName, prNumber)
		if err != nil {
			return errors.Wrapf(err, "failed to lookup PullRequest #%d on repo %s", prNumber, repoFullName)
		}
		return nil
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 3 * time.Second
	bo.MaxElapsedTime = time.Minute
	bo.Reset()
	err = backoff.Retry(f, bo)
	if err != nil {
		return pr, err
	}

	return pr, nil
}

// CreateScmClient creates a new scm client
func (o *EnvironmentPullRequestOptions) CreateScmClient(gitServer, _, gitKind string) (*scm.Client, string, error) {
	if gitKind == "" {
		var err error
		gitKind, err = scmhelpers.DiscoverGitKind(o.JXClient, o.Namespace, gitServer)
		if err != nil {
			return nil, "", errors.Wrapf(err, "failed to discover the git kind for git server %s", gitServer)
		}
	}

	o.ScmClientFactory.GitKind = gitKind

	// lets avoid recreating git clients in unit tests
	if o.ScmClientFactory.GitServerURL == gitServer && o.ScmClientFactory.ScmClient != nil {
		return o.ScmClientFactory.ScmClient, o.ScmClientFactory.GitToken, nil
	}
	o.ScmClientFactory.GitServerURL = gitServer
	scmClient, err := o.ScmClientFactory.Create()
	if err != nil {
		return scmClient, "", errors.Wrapf(err, "failed to create SCM client for server %s", gitServer)
	}
	return scmClient, o.ScmClientFactory.GitToken, nil
}
