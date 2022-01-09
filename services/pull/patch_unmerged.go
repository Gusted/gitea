// Copyright 2021 The Gitea Authors.
// All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package pull

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/log"
)

// lsFileLine is a Quadruplet struct (+error) representing a partially parsed line from ls-files
type lsFileLine struct {
	mode  string
	sha   string
	stage int
	path  string
	err   error
}

// SameAs checks if two lsFileLines are referring to the same path, sha and mode (ignoring stage)
func (line *lsFileLine) SameAs(other *lsFileLine) bool {
	if line == nil || other == nil {
		return false
	}

	if line.err != nil || other.err != nil {
		return false
	}

	return line.mode == other.mode &&
		line.sha == other.sha &&
		line.path == other.path
}

// readUnmergedLsFileLines calls git ls-files -u -z and parses the lines into mode-sha-stage-path quadruplets
// it will push these to the provided channel closing it at the end
func readUnmergedLsFileLines(ctx context.Context, tmpBasePath string, outputChan chan *lsFileLine) {
	defer func() {
		// Always close the outputChan at the end of this function
		close(outputChan)
	}()

	lsFilesReader, lsFilesWriter, err := os.Pipe()
	if err != nil {
		log.Error("Unable to open stderr pipe: %v", err)
		outputChan <- &lsFileLine{err: fmt.Errorf("unable to open stderr pipe: %v", err)}
		return
	}
	defer func() {
		_ = lsFilesWriter.Close()
		_ = lsFilesReader.Close()
	}()

	stderr := &strings.Builder{}
	err = git.NewCommandContext(ctx, "ls-files", "-u", "-z").
		RunInDirTimeoutEnvFullPipelineFunc(
			nil, -1, tmpBasePath,
			lsFilesWriter, stderr, nil,
			func(_ context.Context, _ context.CancelFunc) error {
				_ = lsFilesWriter.Close()
				defer func() {
					_ = lsFilesReader.Close()
				}()
				bufferedReader := bufio.NewReader(lsFilesReader)

				for {
					line, err := bufferedReader.ReadString('\000')
					if err != nil {
						if err == io.EOF {
							return nil
						}
						return err
					}
					toemit := &lsFileLine{}

					split := strings.SplitN(line, " ", 3)
					if len(split) < 3 {
						return fmt.Errorf("malformed line: %s", line)
					}
					toemit.mode = split[0]
					toemit.sha = split[1]

					if len(split[2]) < 4 {
						return fmt.Errorf("malformed line: %s", line)
					}

					toemit.stage, err = strconv.Atoi(split[2][0:1])
					if err != nil {
						return fmt.Errorf("malformed line: %s", line)
					}

					toemit.path = split[2][2 : len(split[2])-1]
					outputChan <- toemit
				}
			})

	if err != nil {
		outputChan <- &lsFileLine{err: fmt.Errorf("git ls-files -u -z: %v", git.ConcatenateError(err, stderr.String()))}
	}
}

// unmergedFile is triple (+error) of lsFileLines split into stages 1,2 & 3.
type unmergedFile struct {
	stage1 *lsFileLine
	stage2 *lsFileLine
	stage3 *lsFileLine
	err    error
}

// unmergedFiles will collate the output from readUnstagedLsFileLines in to file triplets and send them
// to the provided channel, closing at the end.
func unmergedFiles(ctx context.Context, tmpBasePath string, unmerged chan *unmergedFile) {
	defer func() {
		// Always close the channel
		close(unmerged)
	}()

	ctx, cancel := context.WithCancel(ctx)
	lsFileLineChan := make(chan *lsFileLine, 10) // give lsFileLineChan a buffer
	go readUnmergedLsFileLines(ctx, tmpBasePath, lsFileLineChan)
	defer func() {
		cancel()
		for range lsFileLineChan {
			// empty channel
		}
	}()

	next := &unmergedFile{}
	for line := range lsFileLineChan {
		if line.err != nil {
			log.Error("Unable to run ls-files -u -z! Error: %v", line.err)
			unmerged <- &unmergedFile{err: fmt.Errorf("unable to run ls-files -u -z! Error: %v", line.err)}
			return
		}

		// stages are always emitted 1,2,3 but sometimes 1, 2 or 3 are dropped
		switch line.stage {
		case 0:
			// Should not happen as this represents successfully merged file - we will tolerate and ignore though
		case 1:
			if next.stage1 != nil {
				// We need to handle the unstaged file stage1,stage2,stage3
				unmerged <- next
			}
			next = &unmergedFile{stage1: line}
		case 2:
			if next.stage3 != nil || next.stage2 != nil || (next.stage1 != nil && next.stage1.path != line.path) {
				// We need to handle the unstaged file stage1,stage2,stage3
				unmerged <- next
				next = &unmergedFile{}
			}
			next.stage2 = line
		case 3:
			if next.stage3 != nil || (next.stage1 != nil && next.stage1.path != line.path) || (next.stage2 != nil && next.stage2.path != line.path) {
				// We need to handle the unstaged file stage1,stage2,stage3
				unmerged <- next
				next = &unmergedFile{}
			}
			next.stage3 = line
		default:
			log.Error("Unexpected stage %d for path %s in run ls-files -u -z!", line.stage, line.path)
			unmerged <- &unmergedFile{err: fmt.Errorf("unexpected stage %d for path %s in git ls-files -u -z", line.stage, line.path)}
			return
		}
	}
	// We need to handle the unstaged file stage1,stage2,stage3
	if next.stage1 != nil || next.stage2 != nil || next.stage3 != nil {
		unmerged <- next
	}
}
