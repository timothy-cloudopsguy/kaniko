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

	cmd := exec.Command(newCommand[0], newCommand[1:]...)

	cmd.Dir = setWorkDirIfExists(config.WorkingDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	replacementEnvs := buildArgs.ReplacementEnvs(config.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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

	// If specified, run the command as a specific user
	if userStr != "" {
		cmd.SysProcAttr.Credential, err = util.SyscallCredentials(userStr)
		if err != nil {
			return errors.Wrap(err, "credentials")
		}
	}

	env, err := addDefaultHOME(userStr, replacementEnvs)
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
