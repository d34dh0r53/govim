// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"regexp"

	"github.com/kr/pty"
	"github.com/myitcv/govim/testdriver"
	"github.com/rogpeppe/go-internal/testscript"
	"gopkg.in/retry.v1"
)

const (
	keyErrLog = "errLog"
)

var (
	fLogGovim = flag.Bool("govimLog", false, "whether to log govim activity")
)

func TestMain(m *testing.M) {
	os.Exit(testscript.RunMain(m, map[string]func() int{
		"vim":     testdriver.Vim,
		"execvim": execvim,
	}))
}

func TestScripts(t *testing.T) {
	var waitLock sync.Mutex
	var waitList []func() error

	td, err := ioutil.TempDir("", "gobin-gopls-installdir")
	if err != nil {
		t.Fatalf("failed to create temp install directory for gopls: %v", err)
	}
	defer os.RemoveAll(td)

	cmd := exec.Command("go", "install", raceOrNot(), "golang.org/x/tools/cmd/gopls")
	cmd.Env = append(os.Environ(), "GOBIN="+td)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to install temp version of golang.org/x/tools/cmd/gopls: %v\n%s", err, out)
	}

	goplspath := filepath.Join(td, "gopls")
	govimPath := strings.TrimSpace(runCmd(t, "go", "list", "-m", "-f={{.Dir}}"))

	t.Run("scripts", func(t *testing.T) {
		testscript.Run(t, testscript.Params{
			Dir: "testdata",
			Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
				"sleep":      testdriver.Sleep,
				"errlogwait": errLogWait,
			},
			Condition: testdriver.Condition,
			Setup: func(e *testscript.Env) error {
				// We set a special TMPDIR so the file watcher ignores it
				tmp := filepath.Join(e.WorkDir, "_tmp")
				if err := os.MkdirAll(tmp, 0777); err != nil {
					return fmt.Errorf("failed to create temp dir %v: %v", tmp, err)
				}
				home := filepath.Join(e.WorkDir, "home")
				e.Vars = append(e.Vars,
					"TMPDIR="+tmp,
					"HOME="+home,
					"PLUGIN_PATH="+govimPath,
					"CURRENT_GOPATH="+os.Getenv("GOPATH"),
				)
				testPluginPath := filepath.Join(e.WorkDir, "home", ".vim", "pack", "plugins", "start", "govim")
				d := newplugin(string(goplspath))
				td, err := testdriver.NewTestDriver(filepath.Base(e.WorkDir), govimPath, home, testPluginPath, e, d)
				if err != nil {
					t.Fatalf("failed to create new driver: %v", err)
				}
				errLog := new(lockingBuffer)
				td.Log = io.MultiWriter(
					errLog,
					// os.Stderr,
				)
				e.Values[keyErrLog] = errLog
				if *fLogGovim {
					tf, err := ioutil.TempFile("", "govim_test_script_govim_log*")
					if err != nil {
						t.Fatalf("failed to create govim log file: %v", err)
					}
					td.Log = tf
					t.Logf("logging %v to %v\n", filepath.Base(e.WorkDir), tf.Name())
				}
				td.Run()
				waitLock.Lock()
				waitList = append(waitList, td.Wait)
				waitLock.Unlock()
				e.Defer(func() {
					td.Close()
				})
				return nil
			},
		})
	})

	var errLock sync.Mutex
	var errors []error

	var wg sync.WaitGroup

	for _, w := range waitList {
		w := w
		wg.Add(1)
		go func() {
			if err := w(); err != nil {
				errLock.Lock()
				errors = append(errors, err)
				errLock.Unlock()
			}
			wg.Done()
		}()
	}

	wg.Wait()

	if len(errors) > 0 {
		var msgs []string
		for _, e := range errors {
			msgs = append(msgs, e.Error())
		}
		t.Fatalf("got some errors:\n%v\n", strings.Join(msgs, "\n"))
	}
}

func TestInstallScripts(t *testing.T) {
	if testing.Short() {
		t.Skip("Install scripts are long-running")
	}

	govimPath := strings.TrimSpace(runCmd(t, "go", "list", "-m", "-f={{.Dir}}"))

	t.Run("scripts", func(t *testing.T) {
		testscript.Run(t, testscript.Params{
			Dir: "testdatainstall",
			Setup: func(e *testscript.Env) error {
				e.Vars = append(e.Vars,
					"PLUGIN_PATH="+govimPath,
					"CURRENT_GOPATH="+os.Getenv("GOPATH"),
				)
				return nil
			},
		})
	})
}

func runCmd(t *testing.T, c string, args ...string) string {
	t.Helper()
	cmd := exec.Command(c, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("failed to run %v: %v\n%s", strings.Join(cmd.Args, " "), err, out)
	}
	return string(out)
}

func execvim() int {
	args := append([]string{"--not-a-term"}, os.Args[1:]...)
	cmd := exec.Command("vim", args[1:]...)
	thepty, err := pty.Start(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start %v: %v", strings.Join(cmd.Args, " "), err)
		return 1
	}
	go io.Copy(ioutil.Discard, thepty)
	if err := cmd.Wait(); err != nil {
		return 1
	}
	return 0
}

type lockingBuffer struct {
	lock sync.Mutex
	und  bytes.Buffer
}

func (l *lockingBuffer) Write(p []byte) (n int, err error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.und.Write(p)
}

func (l *lockingBuffer) Bytes() []byte {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.und.Bytes()
}

func errLogWait(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("errlogwait does not support negation")
	}
	errLogV := ts.Value(keyErrLog)
	if errLogV == nil {
		ts.Fatalf("errlogwait failed to find errLog")
	}
	errLog, ok := errLogV.(*lockingBuffer)
	if !ok {
		ts.Fatalf("errlogwait errLog was not the right type")
	}

	if len(args) != 1 {
		ts.Fatalf("errlogwait expects a single argument, the regexp to search for")
	}

	reg, err := regexp.Compile(args[0])
	if err != nil {
		ts.Fatalf("errlogwait failed to regexp.Compile %q: %v", args[0], err)
	}

	strategy := retry.LimitTime(30*time.Second,
		retry.Exponential{
			Initial: 10 * time.Millisecond,
			Factor:  1.5,
		},
	)

	found := false
	for a := retry.Start(strategy, nil); a.Next(); {
		if reg.Find(errLog.Bytes()) != nil {
			found = true
			break
		}
	}

	if !found {
		ts.Fatalf("errlogwait failed to find %q; timed out", args[0])
	}
}
