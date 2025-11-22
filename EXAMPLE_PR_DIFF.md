# Getting PR Diff/File Changes

This document demonstrates how to use the new `GetPullRequestDiff` functionality to retrieve detailed file changes including diff content for a pull request.

## Overview

The `GetPullRequestDiff` method returns detailed information about file changes in a pull request, including:
- File names (current and previous if renamed)
- Change status (added, modified, removed, renamed)
- Line additions and deletions counts
- The actual diff/patch content in unified diff format

## Usage Example

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/jfrog/froggit-go/vcsclient"
    "github.com/jfrog/froggit-go/vcsutils"
)

func main() {
    ctx := context.Background()

    // Create a GitHub client
    client, err := vcsclient.NewClientBuilder(vcsutils.GitHub).
        ApiEndpoint("https://api.github.com").
        Token("your-github-token").
        Build()
    if err != nil {
        log.Fatal(err)
    }

    // Get PR diff for pull request #123
    owner := "your-org"
    repo := "your-repo"
    prNumber := 123

    fileChanges, err := client.GetPullRequestDiff(ctx, owner, repo, prNumber)
    if err != nil {
        log.Fatal(err)
    }

    // Display the file changes
    fmt.Printf("Pull Request #%d has %d file changes:\n\n", prNumber, len(fileChanges))

    for _, fc := range fileChanges {
        fmt.Printf("File: %s\n", fc.Filename)
        fmt.Printf("  Status: %s\n", fc.Status)
        fmt.Printf("  Additions: +%d lines\n", fc.Additions)
        fmt.Printf("  Deletions: -%d lines\n", fc.Deletions)

        if fc.PreviousFilename != "" && fc.PreviousFilename != fc.Filename {
            fmt.Printf("  Renamed from: %s\n", fc.PreviousFilename)
        }

        if fc.Patch != "" {
            fmt.Printf("  Diff:\n")
            fmt.Println(fc.Patch)
        }
        fmt.Println()
    }
}
```

## FileChange Structure

The `FileChange` struct contains the following fields:

```go
type FileChange struct {
    // Filename is the current name of the file
    Filename string

    // PreviousFilename is the previous name if the file was renamed
    PreviousFilename string

    // Status indicates the type of change: "added", "modified", "removed", "renamed"
    Status string

    // Additions is the number of lines added
    Additions int

    // Deletions is the number of lines deleted
    Deletions int

    // Changes is the total number of changes
    Changes int

    // Patch contains the actual diff content in unified diff format
    Patch string
}
```

## VCS Provider Support

### GitHub ✅
- Full support with line statistics and patch content
- Uses the GitHub Compare Commits API

### GitLab ✅
- Full support with patch content
- Line statistics may not be available (set to 0)
- Uses GitLab Merge Request Changes API

### Bitbucket Server ✅
- Basic support with file paths and status
- Line statistics and patch content may be limited
- Uses Bitbucket Diff API

### Bitbucket Cloud ✅
- Basic support with file paths and status
- Line statistics available (additions/deletions)
- Patch content not included in diff stats
- Uses Bitbucket DiffStat API

### Azure Repos ✅
- Basic support with file paths and status
- Line statistics may not be available
- Uses Azure DevOps Commit Diffs API

## Notes

- The `Patch` field contains the unified diff format, which is similar to what `git diff` produces
- Some VCS providers may not provide all fields (line statistics, patch content)
- Large pull requests may have pagination handled automatically
- For GitHub, the method handles rate limiting with automatic retries
