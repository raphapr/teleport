/*
Copyright 2022 Gravitational, Inc.

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

package github

import (
	"context"
	"fmt"

	"github.com/gravitational/trace"

	go_github "github.com/google/go-github/v37/github"
	"golang.org/x/oauth2"
)

type Client struct {
	Client *go_github.Client
	Config
}

type Config struct {
	Token        string
	Organization string
	Repository   string
	Username     string
}

// New returns a new GitHub client.
func New(ctx context.Context, c Config) (*Client, error) {
	err := validateConfig(c)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: c.Token},
	)
	return &Client{
		Client: go_github.NewClient(oauth2.NewClient(ctx, ts)),
		Config: c,
	}, nil
}

// validateConfig validates config.
func validateConfig(c Config) error {
	if c.Token == "" {
		return trace.BadParameter("missing token")
	}
	if c.Organization == "" {
		return trace.BadParameter("missing organization")
	}
	if c.Repository == "" {
		return trace.BadParameter("missing repository")
	}
	if c.Username == "" {
		return trace.BadParameter("missing username")
	}
	return nil
}

// Backport backports changes from backportBranchName to a new branch based
// off baseBranchName.
//
// A new branch is created with the name in the format of
// auto-backport/[baseBranchName]/[backportBranchName], and
// cherry-picks commits onto the new branch.
func (c *Client) Backport(ctx context.Context, baseBranchName, backportBranchName string, commits []*go_github.Commit) (string, error) {
	newBranchName := fmt.Sprintf("auto-backport/%s/%s", baseBranchName, backportBranchName)
	// Create a new branch off of the target branch.
	err := c.createBranchFrom(ctx, baseBranchName, newBranchName)
	if err != nil {
		return "", trace.Wrap(err)
	}
	fmt.Printf("Created a new branch: %s.\n", newBranchName)

	// Cherry pick commits.
	err = c.cherryPickCommitsOnBranch(ctx, newBranchName, commits)
	if err != nil {
		return "", trace.Wrap(err)
	}
	fmt.Printf("Finished cherry-picking %v commits. \n", len(commits))
	return newBranchName, nil
}

// cherryPickCommitsOnBranch cherry picks a list of commits on a given branch.
func (c *Client) cherryPickCommitsOnBranch(ctx context.Context, branchName string, commits []*go_github.Commit) error {
	branch, err := c.getBranch(ctx, branchName)
	if err != nil {
		return trace.Wrap(err)
	}
	headCommit, err := c.getCommit(ctx, *branch.Commit.SHA)
	if err != nil {
		return trace.Wrap(err)
	}
	for i := 0; i < len(commits); i++ {
		tree, sha, err := c.cherryPickCommit(ctx, branchName, commits[i], headCommit)
		if err != nil {
			defer c.deleteBranch(ctx, branchName)
			return trace.Wrap(err)
		}
		headCommit.SHA = &sha
		headCommit.Tree = tree
	}
	return nil
}

// cherryPickCommit cherry picks a single commit on a branch.
func (c *Client) cherryPickCommit(ctx context.Context, branchName string, cherryCommit *go_github.Commit, headBranchCommit *go_github.Commit) (*go_github.Tree, string, error) {
	if len(cherryCommit.Parents) != 1 {
		return nil, "", trace.BadParameter("merge commits are not supported")
	}
	cherryParent := cherryCommit.Parents[0]
	// Temporarily set the parent of the branch to the parent of the commit
	// to cherry-pick so they are siblings. When git performs the merge, it
	// detects that the parent of the branch commit we're merging onto matches
	// the parent of the commit we're merging with, and merges a tree of size 1,
	// containing only the cherry-pick commit.
	err := c.createSiblingCommit(ctx, branchName, headBranchCommit, cherryParent)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}

	// Merging the original cherry pick commit onto the branch.
	merge, err := c.merge(ctx, branchName, *cherryCommit.SHA)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	mergeTree := merge.GetTree()

	// Get the updated HEAD commit with the new parent.
	updatedCommit, err := c.getCommit(ctx, *headBranchCommit.SHA)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	// Create a new commit with the updated commit as the parent and the merge tree.
	sha, err := c.createCommit(ctx, *cherryCommit.Message, mergeTree, updatedCommit)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	// Overwrite the merge commit and its parent on the branch by the created commit.
	// The result will be equivalent to what would have happened with a fast-forward merge.
	err = c.updateBranch(ctx, branchName, sha)
	if err != nil {
		return nil, "", trace.Wrap(err)
	}
	return mergeTree, sha, nil
}

// createSiblingCommit creates a commit with the passed in commit's tree and parent
// and updates the passed in branch to point at that commit.
func (c *Client) createSiblingCommit(ctx context.Context, branchName string, branchHeadCommit *go_github.Commit, cherryParent *go_github.Commit) error {
	tree := branchHeadCommit.GetTree()
	// This will be the "temp" commit, commit is lost. Commit message doesn't matter.
	commitSHA, err := c.createCommit(ctx, "temp", tree, cherryParent)
	if err != nil {
		return trace.Wrap(err)
	}
	err = c.updateBranch(ctx, branchName, commitSHA)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// createBranchFrom creates a branch from the passed in branch's HEAD.
func (c *Client) createBranchFrom(ctx context.Context, branchFromName string, newBranchName string) error {
	baseBranch, err := c.getBranch(ctx, branchFromName)
	if err != nil {
		return trace.Wrap(err)
	}
	newRefBranchName := fmt.Sprintf("%s%s", branchRefPrefix, newBranchName)
	baseBranchSHA := baseBranch.GetCommit().GetSHA()

	ref := &go_github.Reference{
		Ref: &newRefBranchName,
		Object: &go_github.GitObject{
			SHA: &baseBranchSHA, /* SHA to branch from */
		},
	}
	_, _, err = c.Client.Git.CreateRef(ctx, c.Organization, c.Repository, ref)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// updateBranch updates a branch.
func (c *Client) updateBranch(ctx context.Context, branchName string, sha string) error {
	refName := fmt.Sprintf("%s%s", branchRefPrefix, branchName)
	_, _, err := c.Client.Git.UpdateRef(ctx, c.Organization, c.Repository, &go_github.Reference{
		Ref: &refName,
		Object: &go_github.GitObject{
			SHA: &sha,
		},
	}, true)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// getBranch gets a branch.
func (c *Client) getBranch(ctx context.Context, branchName string) (*go_github.Branch, error) {
	branch, _, err := c.Client.Repositories.GetBranch(ctx, c.Organization, c.Repository, branchName, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return branch, nil
}

// createCommit creates a new commit.
func (c *Client) createCommit(ctx context.Context, commitMessage string, tree *go_github.Tree, parent *go_github.Commit) (string, error) {
	commit, _, err := c.Client.Git.CreateCommit(ctx, c.Organization, c.Repository, &go_github.Commit{
		Message: &commitMessage,
		Tree:    tree,
		Parents: []*go_github.Commit{
			parent,
		},
	})
	if err != nil {
		return "", trace.Wrap(err)
	}
	return commit.GetSHA(), nil
}

// getCommit gets a commit.
func (c *Client) getCommit(ctx context.Context, sha string) (*go_github.Commit, error) {
	commit, _, err := c.Client.Git.GetCommit(ctx,
		c.Organization,
		c.Repository,
		sha)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return commit, nil
}

// merge merges a branch.
func (c *Client) merge(ctx context.Context, base string, headCommitSHA string) (*go_github.Commit, error) {
	merge, _, err := c.Client.Repositories.Merge(ctx, c.Organization, c.Repository, &go_github.RepositoryMergeRequest{
		Base: &base,
		Head: &headCommitSHA,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	mergeCommit, err := c.getCommit(ctx, merge.GetSHA())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return mergeCommit, nil
}

// GetBranchCommits gets commits on a branch.
//
// The only way to list commits for a branch is through RepositoriesService
// and returns type RepositoryCommit which does not contain the commit
// tree. To get the commit trees, GitService is used to get the commits (of
// type Commit) that contain the commit tree.
func (c *Client) GetBranchCommits(ctx context.Context, branchName string) ([]*go_github.Commit, error) {
	// Getting RepositoryCommits.
	repoCommits, err := c.getBranchCommits(ctx, branchName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the commits that are not on master. No commits will be returned if
	// the pull request from the branch to backport was not squashed and merged
	// or rebased and merged.
	comparison, _, err := c.Client.Repositories.CompareCommits(ctx, c.Organization, c.Repository, "master", branchName)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Getting Commits.
	commits := []*go_github.Commit{}
	for _, repoCommit := range repoCommits {
		for _, diffCommit := range comparison.Commits {
			if diffCommit.GetSHA() == repoCommit.GetSHA() {
				commit, err := c.getCommit(ctx, repoCommit.GetSHA())
				if err != nil {
					return nil, trace.Wrap(err)
				}
				if len(commit.Parents) != 1 {
					return nil, trace.Errorf("merge commits are not supported.")
				}
				commits = append(commits, commit)
			}
		}
	}
	return commits, nil
}

// getBranchCommits gets commits on a branch of type go-github.RepositoryCommit.
func (c *Client) getBranchCommits(ctx context.Context, branchName string) ([]*go_github.RepositoryCommit, error) {
	var repoCommits []*go_github.RepositoryCommit
	listOpts := go_github.ListOptions{
		Page:    0,
		PerPage: perPage,
	}
	opts := &go_github.CommitsListOptions{SHA: branchName, ListOptions: listOpts}
	for {
		currCommits, resp, err := c.Client.Repositories.ListCommits(ctx,
			c.Organization,
			c.Repository,
			opts)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		repoCommits = append(repoCommits, currCommits...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return repoCommits, nil
}

// deleteBranch deletes a branch.
func (c *Client) deleteBranch(ctx context.Context, branchName string) error {
	refName := fmt.Sprintf("%s%s", branchRefPrefix, branchName)
	_, err := c.Client.Git.DeleteRef(ctx, c.Organization, c.Repository, refName)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// CreatePullRequest creates a pull request.
func (c *Client) CreatePullRequest(ctx context.Context, baseBranch string, headBranch string, title string, body string) error {
	autoTitle := fmt.Sprintf("[Auto Backport] %s", title)
	newPR := &go_github.NewPullRequest{
		Title:               &autoTitle,
		Head:                &headBranch,
		Base:                &baseBranch,
		Body:                &body,
		MaintainerCanModify: go_github.Bool(true),
	}
	_, _, err := c.Client.PullRequests.Create(ctx, c.Organization, c.Repository, newPR)
	if err != nil {
		return err
	}
	return nil
}

const (
	backportPRState          = "closed"
	backportMasterBranchName = "master"
)

// GetPullRequestMetadata gets a pull request's title and body by branch name.
func (c *Client) GetPullRequestMetadata(ctx context.Context, branchName string) (title string, body string, err error) {
	prBranchName := fmt.Sprintf("%s:%s", c.Username, branchName)
	prs, _, err := c.Client.PullRequests.List(ctx,
		c.Organization,
		c.Repository,
		&go_github.PullRequestListOptions{
			// Get PRs that are closed and whose base is master.
			State: backportPRState,
			Base:  backportMasterBranchName,
			// Head filters pull requests by user and branch name in the format of:
			// "user:ref-name".
			Head: prBranchName,
		})
	if err != nil {
		return "", "", trace.Wrap(err)
	}
	if len(prs) == 0 {
		return "", "", trace.Errorf("pull request for branch %s does not exist", branchName)
	}
	if len(prs) != 1 {
		return "", "", trace.Errorf("found more than 1 pull request for branch %s", branchName)
	}
	pull := prs[0]
	return pull.GetTitle(), pull.GetBody(), nil
}

const (
	// perPage is the number of items per page to request.
	perPage = 100

	// branchRefPrefix is the prefix for a reference that is
	// pointing to a branch.
	branchRefPrefix = "refs/heads/"
)
