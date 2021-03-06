// Copyright 2015 trivago GmbH
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package producer

import (
	"compress/gzip"
	"fmt"
	"github.com/trivago/gollum/core"
	"github.com/trivago/gollum/core/log"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type fileState struct {
	file         *os.File
	batch        *core.MessageBatch
	bgWriter     *sync.WaitGroup
	fileCreated  time.Time
	flushTimeout time.Duration
}

type fileRotateConfig struct {
	timeout  time.Duration
	sizeByte int64
	atHour   int
	atMinute int
	enabled  bool
	compress bool
}

func newFileState(bufferSizeMax int, timeout time.Duration) *fileState {
	return &fileState{
		batch:        core.NewMessageBatch(bufferSizeMax, nil),
		bgWriter:     new(sync.WaitGroup),
		flushTimeout: timeout,
	}
}

func (state *fileState) flush() {
	state.writeBatch()
	state.batch.WaitForFlush(state.flushTimeout)
	state.bgWriter.Wait()
	state.file.Close()
}

func (state *fileState) compressAndCloseLog(sourceFile *os.File) {
	state.bgWriter.Add(1)
	defer state.bgWriter.Done()

	// Generate file to zip into
	sourceFileName := sourceFile.Name()
	sourceDir := filepath.Dir(sourceFileName)
	sourceExt := filepath.Ext(sourceFileName)
	sourceBase := filepath.Base(sourceFileName)
	sourceBase = sourceBase[:len(sourceBase)-len(sourceExt)]

	targetFileName := fmt.Sprintf("%s/%s.gz", sourceDir, sourceBase)

	targetFile, err := os.OpenFile(targetFileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		Log.Error.Print("File compress error:", err)
		sourceFile.Close()
		return
	}

	// Create zipfile and compress data
	Log.Note.Print("Compressing " + sourceFileName)

	sourceFile.Seek(0, 0)
	targetWriter := gzip.NewWriter(targetFile)

	for err == nil {
		_, err = io.CopyN(targetWriter, sourceFile, 1<<20) // 1 MB chunks
		runtime.Gosched()                                  // Be async!
	}

	// Cleanup
	sourceFile.Close()
	targetWriter.Close()
	targetFile.Close()

	if err != nil && err != io.EOF {
		Log.Warning.Print("Compression failed:", err)
		err = os.Remove(targetFileName)
		if err != nil {
			Log.Error.Print("Compressed file remove failed:", err)
		}
		return
	}

	// Remove original log
	err = os.Remove(sourceFileName)
	if err != nil {
		Log.Error.Print("Uncompressed file remove failed:", err)
	}
}

func (state *fileState) onWriterError(err error) bool {
	Log.Error.Print("File write error:", err)
	return false
}

func (state *fileState) writeBatch() {
	state.batch.Flush(state.file, nil, state.onWriterError)
}

func (state *fileState) needsRotate(rotate fileRotateConfig, forceRotate bool) (bool, error) {
	// File does not exist?
	if state.file == nil {
		return true, nil
	}

	// File can be accessed?
	stats, err := state.file.Stat()
	if err != nil {
		return false, err
	}

	// File needs rotation?
	if !rotate.enabled {
		return false, nil
	}

	if forceRotate {
		return true, nil
	}

	// File is too large?
	if stats.Size() >= rotate.sizeByte {
		return true, nil // ### return, too large ###
	}

	// File is too old?
	if time.Since(state.fileCreated) >= rotate.timeout {
		return true, nil // ### return, too old ###
	}

	// RotateAt crossed?
	if rotate.atHour > -1 && rotate.atMinute > -1 {
		now := time.Now()
		rotateAt := time.Date(now.Year(), now.Month(), now.Day(), rotate.atHour, rotate.atMinute, 0, 0, now.Location())

		if state.fileCreated.Sub(rotateAt).Minutes() < 0 {
			return true, nil // ### return, too old ###
		}
	}

	// nope, everything is ok
	return false, nil
}
