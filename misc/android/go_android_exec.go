// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build ignore
// +build ignore

// This program can be used as go_android_GOARCH_exec by the Go tool.
// It executes binaries on an android device using adb.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

func run(args ...string) (string, error) {
	cmd := adbCmd(args...)
	buf := new(strings.Builder)
	cmd.Stdout = io.MultiWriter(os.Stdout, buf)
	// If the adb subprocess somehow hangs, go test will kill this wrapper
	// and wait for our os.Stderr (and os.Stdout) to close as a result.
	// However, if the os.Stderr (or os.Stdout) file descriptors are
	// passed on, the hanging adb subprocess will hold them open and
	// go test will hang forever.
	//
	// Avoid that by wrapping stderr, breaking the short circuit and
	// forcing cmd.Run to use another pipe and goroutine to pass
	// along stderr from adb.
	cmd.Stderr = struct{ io.Writer }{os.Stderr}
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("adb %s: %v", strings.Join(args, " "), err)
	}
	return buf.String(), nil
}

func adb(args ...string) error {
	if out, err := adbCmd(args...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "adb %s\n%s", strings.Join(args, " "), out)
		return err
	}
	return nil
}

func adbCmd(args ...string) *exec.Cmd {
	if flags := os.Getenv("GOANDROID_ADB_FLAGS"); flags != "" {
		args = append(strings.Split(flags, " "), args...)
	}
	return exec.Command("adb", args...)
}

const (
	deviceRoot   = "/data/local/tmp/go_android_exec"
	deviceGoroot = deviceRoot + "/goroot"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("go_android_exec: ")
	exitCode, err := runMain()
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(exitCode)
}

func runMain() (int, error) {
	// Concurrent use of adb is flaky, so serialize adb commands.
	// See https://github.com/golang/go/issues/23795 or
	// https://issuetracker.google.com/issues/73230216.
	lockPath := filepath.Join(os.TempDir(), "go_android_exec-adb-lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}

	// In case we're booting a device or emulator alongside all.bash, wait for
	// it to be ready. adb wait-for-device is not enough, we have to
	// wait for sys.boot_completed.
	if err := adb("wait-for-device", "exec-out", "while [[ -z $(getprop sys.boot_completed) ]]; do sleep 1; done;"); err != nil {
		return 0, err
	}

	// Done once per make.bash.
	if err := adbCopyGoroot(); err != nil {
		return 0, err
	}

	// Prepare a temporary directory that will be cleaned up at the end.
	// Binary names can conflict.
	// E.g. template.test from the {html,text}/template packages.
	binName := filepath.Base(os.Args[1])
	deviceGotmp := fmt.Sprintf(deviceRoot+"/%s-%d", binName, os.Getpid())
	deviceGopath := deviceGotmp + "/gopath"
	defer adb("exec-out", "rm", "-rf", deviceGotmp) // Clean up.

	// Determine the package by examining the current working
	// directory, which will look something like
	// "$GOROOT/src/mime/multipart" or "$GOPATH/src/golang.org/x/mobile".
	// We extract everything after the $GOROOT or $GOPATH to run on the
	// same relative directory on the target device.
	importPath, isStd, err := pkgPath()
	if err != nil {
		return 0, err
	}
	var deviceCwd string
	if isStd {
		// Note that we use path.Join here instead of filepath.Join:
		// The device paths should be slash-separated even if the go_android_exec
		// wrapper itself is compiled for Windows.
		deviceCwd = path.Join(deviceGoroot, "src", importPath)
	} else {
		deviceCwd = path.Join(deviceGopath, "src", importPath)
		if err := adb("exec-out", "mkdir", "-p", deviceCwd); err != nil {
			return 0, err
		}
		if err := adbCopyTree(deviceCwd, importPath); err != nil {
			return 0, err
		}

		// Copy .go files from the package.
		goFiles, err := filepath.Glob("*.go")
		if err != nil {
			return 0, err
		}
		if len(goFiles) > 0 {
			args := append(append([]string{"push"}, goFiles...), deviceCwd)
			if err := adb(args...); err != nil {
				return 0, err
			}
		}
	}

	deviceBin := fmt.Sprintf("%s/%s", deviceGotmp, binName)
	if err := adb("push", os.Args[1], deviceBin); err != nil {
		return 0, err
	}

	// Forward SIGQUIT from the go command to show backtraces from
	// the binary instead of from this wrapper.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGQUIT)
	go func() {
		for range quit {
			// We don't have the PID of the running process; use the
			// binary name instead.
			adb("exec-out", "killall -QUIT "+binName)
		}
	}()
	// In light of
	// https://code.google.com/p/android/issues/detail?id=3254
	// dont trust the exitcode of adb. Instead, append the exitcode to
	// the output and parse it from there.
	const exitstr = "exitcode="
	cmd := `export TMPDIR="` + deviceGotmp + `"` +
		`; export GOROOT="` + deviceGoroot + `"` +
		`; export GOPATH="` + deviceGopath + `"` +
		`; export CGO_ENABLED=0` +
		`; export GOPROXY=` + os.Getenv("GOPROXY") +
		`; export GOCACHE="` + deviceRoot + `/gocache"` +
		`; export PATH="` + deviceGoroot + `/bin":$PATH` +
		`; cd "` + deviceCwd + `"` +
		"; '" + deviceBin + "' " + strings.Join(os.Args[2:], " ") +
		"; echo -n " + exitstr + "$?"
	output, err := run("exec-out", cmd)
	signal.Reset(syscall.SIGQUIT)
	close(quit)
	if err != nil {
		return 0, err
	}

	exitIdx := strings.LastIndex(output, exitstr)
	if exitIdx == -1 {
		return 0, fmt.Errorf("no exit code: %q", output)
	}
	code, err := strconv.Atoi(output[exitIdx+len(exitstr):])
	if err != nil {
		return 0, fmt.Errorf("bad exit code: %v", err)
	}
	return code, nil
}

// pkgPath determines the package import path of the current working directory,
// and indicates whether it is
// and returns the path to the package source relative to $GOROOT (or $GOPATH).
func pkgPath() (importPath string, isStd bool, err error) {
	goTool, err := goTool()
	if err != nil {
		return "", false, err
	}
	cmd := exec.Command(goTool, "list", "-e", "-f", "{{.ImportPath}}:{{.Standard}}", ".")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", false, fmt.Errorf("%v: %s", cmd, ee.Stderr)
		}
		return "", false, fmt.Errorf("%v: %w", cmd, err)
	}

	s := string(bytes.TrimSpace(out))
	importPath, isStdStr, ok := strings.Cut(s, ":")
	if !ok {
		return "", false, fmt.Errorf("%v: missing ':' in output: %q", cmd, out)
	}
	if importPath == "" || importPath == "." {
		return "", false, fmt.Errorf("current directory does not have a Go import path")
	}
	isStd, err = strconv.ParseBool(isStdStr)
	if err != nil {
		return "", false, fmt.Errorf("%v: non-boolean .Standard in output: %q", cmd, out)
	}

	return importPath, isStd, nil
}

// adbCopyTree copies testdata, go.mod, go.sum files from subdir
// and from parent directories all the way up to the root of subdir.
// go.mod and go.sum files are needed for the go tool modules queries,
// and the testdata directories for tests.  It is common for tests to
// reach out into testdata from parent packages.
func adbCopyTree(deviceCwd, subdir string) error {
	dir := ""
	for {
		for _, name := range []string{"testdata", "go.mod", "go.sum"} {
			hostPath := filepath.Join(dir, name)
			if _, err := os.Stat(hostPath); err != nil {
				continue
			}
			devicePath := path.Join(deviceCwd, dir)
			if err := adb("exec-out", "mkdir", "-p", devicePath); err != nil {
				return err
			}
			if err := adb("push", hostPath, devicePath); err != nil {
				return err
			}
		}
		if subdir == "." {
			break
		}
		subdir = filepath.Dir(subdir)
		dir = path.Join(dir, "..")
	}
	return nil
}

// adbCopyGoroot clears deviceRoot for previous versions of GOROOT, GOPATH
// and temporary data. Then, it copies relevant parts of GOROOT to the device,
// including the go tool built for android.
// A lock file ensures this only happens once, even with concurrent exec
// wrappers.
func adbCopyGoroot() error {
	goTool, err := goTool()
	if err != nil {
		return err
	}
	cmd := exec.Command(goTool, "version")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %w", cmd, err)
	}
	goVersion := string(out)

	// Also known by cmd/dist. The bootstrap command deletes the file.
	statPath := filepath.Join(os.TempDir(), "go_android_exec-adb-sync-status")
	stat, err := os.OpenFile(statPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer stat.Close()
	// Serialize check and copying.
	if err := syscall.Flock(int(stat.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	s, err := io.ReadAll(stat)
	if err != nil {
		return err
	}
	if string(s) == goVersion {
		return nil
	}

	goroot, err := findGoroot()
	if err != nil {
		return err
	}

	// Delete the device's GOROOT, GOPATH and any leftover test data,
	// and recreate GOROOT.
	if err := adb("exec-out", "rm", "-rf", deviceRoot); err != nil {
		return err
	}

	// Build Go for Android.
	cmd = exec.Command(goTool, "install", "cmd")
	out, err = cmd.CombinedOutput()
	if err != nil {
		if len(bytes.TrimSpace(out)) > 0 {
			log.Printf("\n%s", out)
		}
		return fmt.Errorf("%v: %w", cmd, err)
	}
	if err := adb("exec-out", "mkdir", "-p", deviceGoroot); err != nil {
		return err
	}

	// Copy the Android tools from the relevant bin subdirectory to GOROOT/bin.
	cmd = exec.Command(goTool, "list", "-f", "{{.Target}}", "cmd/go")
	cmd.Stderr = os.Stderr
	out, err = cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %w", cmd, err)
	}
	platformBin := filepath.Dir(string(bytes.TrimSpace(out)))
	if platformBin == "." {
		return errors.New("failed to locate cmd/go for target platform")
	}
	if err := adb("push", platformBin, path.Join(deviceGoroot, "bin")); err != nil {
		return err
	}

	// Copy only the relevant subdirectories from pkg: pkg/include and the
	// platform-native binaries in pkg/tool.
	if err := adb("exec-out", "mkdir", "-p", path.Join(deviceGoroot, "pkg", "tool")); err != nil {
		return err
	}
	if err := adb("push", filepath.Join(goroot, "pkg", "include"), path.Join(deviceGoroot, "pkg", "include")); err != nil {
		return err
	}

	cmd = exec.Command(goTool, "list", "-f", "{{.Target}}", "cmd/compile")
	cmd.Stderr = os.Stderr
	out, err = cmd.Output()
	if err != nil {
		return fmt.Errorf("%v: %w", cmd, err)
	}
	platformToolDir := filepath.Dir(string(bytes.TrimSpace(out)))
	if platformToolDir == "." {
		return errors.New("failed to locate cmd/compile for target platform")
	}
	relToolDir, err := filepath.Rel(filepath.Join(goroot), platformToolDir)
	if err != nil {
		return err
	}
	if err := adb("push", platformToolDir, path.Join(deviceGoroot, relToolDir)); err != nil {
		return err
	}

	// Copy all other files from GOROOT.
	dirents, err := os.ReadDir(goroot)
	if err != nil {
		return err
	}
	for _, de := range dirents {
		switch de.Name() {
		case "bin", "pkg":
			// We already created GOROOT/bin and GOROOT/pkg above; skip those.
			continue
		}
		if err := adb("push", filepath.Join(goroot, de.Name()), path.Join(deviceGoroot, de.Name())); err != nil {
			return err
		}
	}

	if _, err := stat.WriteString(goVersion); err != nil {
		return err
	}
	return nil
}

func findGoroot() (string, error) {
	gorootOnce.Do(func() {
		// If runtime.GOROOT reports a non-empty path, assume that it is valid.
		// (It may be empty if this binary was built with -trimpath.)
		gorootPath = runtime.GOROOT()
		if gorootPath != "" {
			return
		}

		// runtime.GOROOT is empty — perhaps go_android_exec was built with
		// -trimpath and GOROOT is unset. Try 'go env GOROOT' as a fallback,
		// assuming that the 'go' command in $PATH is the correct one.

		cmd := exec.Command("go", "env", "GOROOT")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			gorootErr = fmt.Errorf("%v: %w", cmd, err)
		}

		gorootPath = string(bytes.TrimSpace(out))
		if gorootPath == "" {
			gorootErr = errors.New("GOROOT not found")
		}
	})

	return gorootPath, gorootErr
}

func goTool() (string, error) {
	goroot, err := findGoroot()
	if err != nil {
		return "", err
	}
	return filepath.Join(goroot, "bin", "go"), nil
}

var (
	gorootOnce sync.Once
	gorootPath string
	gorootErr  error
)
