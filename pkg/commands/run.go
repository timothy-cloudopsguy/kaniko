/*
Copyright 2018 Google LLC

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

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	kConfig "github.com/chainguard-dev/kaniko/pkg/config"
	"github.com/chainguard-dev/kaniko/pkg/constants"
	"github.com/chainguard-dev/kaniko/pkg/dockerfile"
	"github.com/chainguard-dev/kaniko/pkg/util"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type RunCommand struct {
	BaseCommand
	cmd      *instructions.RunCommand
	shdCache bool
}

// for testing
var (
	userLookup = util.LookupUser
)

func (r *RunCommand) IsArgsEnvsRequiredInCache() bool {
	return true
}

func (r *RunCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	return runCommandInExec(config, buildArgs, r.cmd)
}

func runCommandInExec(config *v1.Config, buildArgs *dockerfile.BuildArgs, cmdRun *instructions.RunCommand) error {
	var newCommand []string
	if cmdRun.PrependShell {
		// This is the default shell on Linux
		var shell []string
		if len(config.Shell) > 0 {
			shell = config.Shell
		} else {
			shell = append(shell, "/bin/sh", "-c")
		}

		newCommand = append(shell, strings.Join(cmdRun.CmdLine, " "))
	} else {
		newCommand = cmdRun.CmdLine
		// Find and set absolute path of executable by setting PATH temporary
		replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
		for _, v := range replacementEnvs {
			entry := strings.SplitN(v, "=", 2)
			if entry[0] != "PATH" {
				continue
			}
			oldPath := os.Getenv("PATH")
			defer os.Setenv("PATH", oldPath)
			os.Setenv("PATH", entry[1])
			path, err := exec.LookPath(newCommand[0])
			if err == nil {
				newCommand[0] = path
			}
		}
	}

	logrus.Infof("Cmd: %s", newCommand[0])
	logrus.Infof("Args: %s", newCommand[1:])

	// Resolve user before setting up PRoot
	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
	u := config.User
	userAndGroup := strings.Split(u, ":")
	userStr, err := util.ResolveEnvironmentReplacement(userAndGroup[0], replacementEnvs, false)
	if err != nil {
		return errors.Wrapf(err, "resolving user %s", userAndGroup[0])
	}

	// Check if we're in Lambda mode first
	lambdaMode := false
	if val, ok := os.LookupEnv("KANIKO_LAMBDA_MODE"); ok {
		if val == "true" || val == "1" {
			lambdaMode = true
		}
	}

	if lambdaMode {
		// Execute command through our Lambda-compatible wrapper
		logrus.Infof("Running in Lambda mode with filesystem interception")

		// Use our command wrapper script
		wrapperScript := "/kaniko_command_wrapper.sh"
		if _, err := os.Stat(wrapperScript); os.IsNotExist(err) {
			return errors.Errorf("command wrapper script not found at %s", wrapperScript)
		}

		// Build the command to execute
		var cmd *exec.Cmd
		if cmdRun.PrependShell {
			// For shell commands, pass the entire command line to the wrapper
			cmd = exec.Command(wrapperScript, "/bin/sh", "-c", strings.Join(cmdRun.CmdLine, " "))
		} else {
			// For direct commands, pass the command and arguments to the wrapper
			args := append([]string{newCommand[0]}, newCommand[1:]...)
			cmd = exec.Command(wrapperScript, args...)
		}

		// Set environment variables
		replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
		cmd.Env = replacementEnvs

		// Set kaniko directory as working directory
		cmd.Dir = kConfig.KanikoDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Add Lambda-specific environment variables
		cmd.Env = append(cmd.Env, "KANIKO_LAMBDA_MODE=true")
		cmd.Env = append(cmd.Env, fmt.Sprintf("KANIKO_BASE=%s", kConfig.KanikoDir))

		// Set user if specified (though Lambda mode may not support this well)
		if userStr != "" && userStr != "root" && userStr != "0" {
			logrus.Warnf("Running as user %s in Lambda mode - this may not work as expected", userStr)
		}

		logrus.Infof("Running Lambda command: %s", cmd.Args)
		if err := cmd.Start(); err != nil {
			return errors.Wrap(err, "starting lambda command")
		}

		// Wait for the process to complete
		if err := cmd.Wait(); err != nil {
			return errors.Wrap(err, "waiting for lambda command to exit")
		}

		return nil
	}

	// Check if we should use PRoot or run commands directly
	// In containerized environments, PRoot may fail due to ptrace restrictions
	useProot := true
	if val, ok := os.LookupEnv("KANIKO_USE_PROOT"); ok {
		if val == "false" || val == "0" {
			useProot = false
		}
	}

	if !useProot {
		// Execute command directly without PRoot
		logrus.Infof("Executing command directly (PRoot disabled)")
		var cmd *exec.Cmd
		if cmdRun.PrependShell {
			cmd = exec.Command("/bin/sh", "-c", strings.Join(cmdRun.CmdLine, " "))
		} else {
			cmd = exec.Command(newCommand[0], newCommand[1:]...)
		}

		// Set environment variables
		replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
		cmd.Env = replacementEnvs
		cmd.Dir = kConfig.KanikoDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// Set user if specified
		if userStr != "" && userStr != "root" && userStr != "0" {
			// Note: Running as non-root user without PRoot may not work in all cases
			logrus.Warnf("Running as user %s without PRoot - this may fail if user doesn't exist", userStr)
		}

		return cmd.Run()
	}

	// Use PRoot to execute commands in the extracted filesystem context
	// This isolates the command execution from the original container environment
	// PRoot provides better compatibility and doesn't require root privileges
	logrus.Infof("Using PRoot for command execution")
	var prootArgs []string
	prootArgs = append(prootArgs, "-R", kConfig.KanikoDir)

	// Add essential bind mounts for system functionality
	// This ensures /proc, /sys, /dev are available in the guest environment
	prootArgs = append(prootArgs, "-b", "/proc", "-b", "/sys", "-b", "/dev", "-b", "/tmp")

	// Bind common host binaries that might be needed in the guest
	// This ensures commands like wget, unzip, make, etc. are available
	commonBinaries := []string{"wget", "unzip", "tar", "make", "dnf", "yum", "apt", "apt-get", "curl", "git", "golang", "go"}
	for _, binary := range commonBinaries {
		if hostPath, err := exec.LookPath(binary); err == nil {
			prootArgs = append(prootArgs, "-b", hostPath)
		}
	}

	// Also bind common shell utilities
	shellUtils := []string{"bash", "sh", "cat", "ls", "mkdir", "rm", "cp", "mv", "chmod", "chown"}
	for _, util := range shellUtils {
		if hostPath, err := exec.LookPath(util); err == nil {
			prootArgs = append(prootArgs, "-b", hostPath)
		}
	}

	// For package managers and system operations, fake root privileges
	// This allows dnf, apt, etc. to work properly without actual root access
	if userStr == "" || userStr == "root" || userStr == "0" {
		prootArgs = append(prootArgs, "-0")
	} else if userStr != "" {
		// For specific users, use PRoot's user switching
		prootArgs = append(prootArgs, "-u", userStr)
	}

	// Set up environment for PRoot execution
	// Convert the environment map to slice format for command execution
	var prootEnvSlice []string
	for key, value := range buildArgs.ReplacementEnvs(config.Env) {
		prootEnvSlice = append(prootEnvSlice, fmt.Sprintf("%s=%s", key, value))
	}

	// Disable seccomp in PRoot to avoid ptrace restrictions in containerized environments
	prootEnvSlice = append(prootEnvSlice, "PROOT_NO_SECCOMP=1")

	// Ensure PATH includes common binary directories within the guest
	// and the directories where bound binaries are available
	pathFound := false
	for i, env := range prootEnvSlice {
		if strings.HasPrefix(env, "PATH=") {
			prootEnvSlice[i] = env + ":/bin:/usr/bin:/sbin:/usr/sbin:/usr/local/bin:."
			pathFound = true
			break
		}
	}
	if !pathFound {
		prootEnvSlice = append(prootEnvSlice, "PATH=/bin:/usr/bin:/sbin:/usr/sbin:/usr/local/bin:.")
	}

	// Set HOME to a writable location within the guest
	prootEnvSlice = append(prootEnvSlice, fmt.Sprintf("HOME=%s", kConfig.KanikoDir))

	// Set TMPDIR to ensure temporary files go to the right place
	prootEnvSlice = append(prootEnvSlice, "TMPDIR=/tmp")

	var cmd *exec.Cmd
	if cmdRun.PrependShell {
		// For shell commands, use the shell from the host and bind it into the guest
		// Use the configured shell or default to /bin/sh from host
		shell := "/bin/sh"
		if len(config.Shell) > 0 {
			shell = config.Shell[0]
		}

		// Bind the host shell into the guest filesystem and use it
		prootArgs = append(prootArgs, "-b", shell)
		cmd = exec.Command("proot", append(prootArgs, filepath.Base(shell), "-c", strings.Join(cmdRun.CmdLine, " "))...)
	} else {
		// For direct commands, use PRoot with automatic binding
		// Ensure the command path is accessible - use host path since guest might not have it
		guestCommand := newCommand[0]
		if strings.HasPrefix(guestCommand, "/") {
			// If it's an absolute path, bind the host binary if it exists
			if _, err := os.Stat(guestCommand); err == nil {
				prootArgs = append(prootArgs, "-b", guestCommand)
				guestCommand = filepath.Base(guestCommand)
			}
		} else {
			// If it's not an absolute path, look for it in the host PATH first
			if hostPath, err := exec.LookPath(guestCommand); err == nil {
				// Bind the host binary into the guest filesystem
				prootArgs = append(prootArgs, "-b", hostPath)
				guestCommand = filepath.Base(hostPath)
			}
		}

		cmd = exec.Command("proot", append(prootArgs, guestCommand)...)
		cmd.Args = append(cmd.Args, newCommand[1:]...)
	}

	// Set working directory - ensure it exists within the guest filesystem
	workDir := kConfig.KanikoDir // Default to kaniko directory
	if config.WorkingDir != "" && config.WorkingDir != "/" {
		// Check if the working directory exists in the guest, fallback to kaniko dir
		if _, err := os.Stat(filepath.Join(kConfig.KanikoDir, config.WorkingDir)); err == nil {
			workDir = config.WorkingDir
		} else {
			logrus.Warnf("Working directory %s not found in guest filesystem, using %s", config.WorkingDir, kConfig.KanikoDir)
		}
	}
	cmd.Dir = workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// User switching is handled by PRoot with -0 or -u flags above
	// No need for host-level credential changes

	// Set environment variables for PRoot execution
	env, err := addDefaultHOME(userStr, prootEnvSlice)
	if err != nil {
		return errors.Wrap(err, "adding default HOME variable")
	}
	cmd.Env = env

	logrus.Infof("Running: %s", cmd.Args)
	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, "starting command")
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return errors.Wrap(err, "getting group id for process")
	}
	if err := cmd.Wait(); err != nil {
		return errors.Wrap(err, "waiting for process to exit")
	}

	//it's not an error if there are no grandchildren
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err.Error() != "no such process" {
		return err
	}
	return nil
}

// addDefaultHOME adds the default value for HOME if it isn't already set
func addDefaultHOME(u string, envs []string) ([]string, error) {
	for _, env := range envs {
		split := strings.SplitN(env, "=", 2)
		if split[0] == constants.HOME {
			return envs, nil
		}
	}

	// If user isn't set, set default value of HOME
	if u == "" || u == constants.RootUser {
		return append(envs, fmt.Sprintf("%s=%s", constants.HOME, constants.DefaultHOMEValue)), nil
	}

	// If user is set to username, set value of HOME to /home/${user}
	// Otherwise the user is set to uid and HOME is /
	userObj, err := userLookup(u)
	if err != nil {
		return nil, fmt.Errorf("lookup user %v: %w", u, err)
	}

	return append(envs, fmt.Sprintf("%s=%s", constants.HOME, userObj.HomeDir)), nil
}

// String returns some information about the command for the image config
func (r *RunCommand) String() string {
	return r.cmd.String()
}

func (r *RunCommand) FilesToSnapshot() []string {
	return nil
}

func (r *RunCommand) ProvidesFilesToSnapshot() bool {
	return false
}

// CacheCommand returns true since this command should be cached
func (r *RunCommand) CacheCommand(img v1.Image) DockerCommand {

	return &CachingRunCommand{
		img:       img,
		cmd:       r.cmd,
		extractFn: util.ExtractFile,
	}
}

func (r *RunCommand) MetadataOnly() bool {
	return false
}

func (r *RunCommand) RequiresUnpackedFS() bool {
	return true
}

func (r *RunCommand) ShouldCacheOutput() bool {
	return r.shdCache
}

type CachingRunCommand struct {
	BaseCommand
	caching
	img            v1.Image
	extractedFiles []string
	cmd            *instructions.RunCommand
	extractFn      util.ExtractFunction
}

func (cr *CachingRunCommand) IsArgsEnvsRequiredInCache() bool {
	return true
}

func (cr *CachingRunCommand) ExecuteCommand(config *v1.Config, buildArgs *dockerfile.BuildArgs) error {
	logrus.Infof("Found cached layer, extracting to filesystem")
	var err error

	if cr.img == nil {
		return errors.New(fmt.Sprintf("command image is nil %v", cr.String()))
	}

	layers, err := cr.img.Layers()
	if err != nil {
		return errors.Wrap(err, "retrieving image layers")
	}

	if len(layers) != 1 {
		return errors.New(fmt.Sprintf("expected %d layers but got %d", 1, len(layers)))
	}

	cr.layer = layers[0]

	cr.extractedFiles, err = util.GetFSFromLayers(
		kConfig.RootDir,
		layers,
		util.ExtractFunc(cr.extractFn),
		util.IncludeWhiteout(),
	)
	if err != nil {
		return errors.Wrap(err, "extracting fs from image")
	}

	return nil
}

func (cr *CachingRunCommand) FilesToSnapshot() []string {
	f := cr.extractedFiles
	logrus.Debugf("%d files extracted by caching run command", len(f))
	logrus.Tracef("Extracted files: %s", f)

	return f
}

func (cr *CachingRunCommand) String() string {
	if cr.cmd == nil {
		return "nil command"
	}
	return cr.cmd.String()
}

func (cr *CachingRunCommand) MetadataOnly() bool {
	return false
}

// todo: this should create the workdir if it doesn't exist, atleast this is what docker does
func setWorkDirIfExists(workdir string) string {
	if _, err := os.Lstat(workdir); err == nil {
		return workdir
	}
	return ""
}
