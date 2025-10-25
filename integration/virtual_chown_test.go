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

package integration

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestVirtualChownBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Skip if no Docker daemon
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker not available")
	}

	// Build context path
	contextPath := "dockerfiles-with-context/virtual-chown-test"

	// Check if test context exists
	contextFullPath := filepath.Join(getCwd(), contextPath)
	if _, err := os.Stat(contextFullPath); os.IsNotExist(err) {
		t.Skip("Test context not found")
	}

	// Set environment variable for virtual chown
	os.Setenv("KANIKO_VIRTUAL_CHOWN", "true")
	defer os.Unsetenv("KANIKO_VIRTUAL_CHOWN")

	// Build with virtual chown enabled
	imageName := "kaniko-virtual-chown-test:latest"

	// Run kaniko executor with virtual chown
	cmd := exec.Command("docker", "run", "--rm", "-v", fmt.Sprintf("%s:/workspace", contextFullPath),
		"-e", "KANIKO_VIRTUAL_CHOWN=true",
		"gcr.io/kaniko-project/executor:latest",
		"--dockerfile", "Dockerfile",
		"--context", "/workspace",
		"--destination", imageName,
		"--no-push",
		"--tar-path", "/tmp/test-image.tar")

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("Kaniko build output: %s", string(output))
		t.Fatalf("Failed to build image with virtual chown: %v", err)
	}

	// Verify the tar file was created
	tarPath := "/tmp/test-image.tar"
	if _, err := os.Stat(tarPath); os.IsNotExist(err) {
		t.Fatal("Expected tar file not found")
	}

	// Extract and verify ownership in tar
	verifyVirtualOwnershipInTar(t, tarPath)

	t.Log("Virtual chown integration test passed!")
}

func verifyVirtualOwnershipInTar(t *testing.T, tarPath string) {
	// Open the tar file
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("Failed to open tar file: %v", err)
	}
	defer f.Close()

	// Read tar entries
	tr := tar.NewReader(f)
	foundFiles := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Failed to read tar header: %v", err)
		}

		// Check if this is one of our test files
		if strings.Contains(hdr.Name, "test.txt") || strings.Contains(hdr.Name, "config.json") {
			foundFiles++

			// In virtual chown mode, these files should have specific ownership
			// test.txt should be owned by testuser (1001:1001) based on COPY --chown directive
			// config.json should be owned by 1001:1001 based on COPY --chown=1001:1001

			if strings.Contains(hdr.Name, "test.txt") {
				if hdr.Uid != 1001 || hdr.Gid != 1001 {
					t.Errorf("Expected test.txt to have UID=1001, GID=1001 in tar, got UID=%d, GID=%d", hdr.Uid, hdr.Gid)
				}
			}

			if strings.Contains(hdr.Name, "config.json") {
				if hdr.Uid != 1001 || hdr.Gid != 1001 {
					t.Errorf("Expected config.json to have UID=1001, GID=1001 in tar, got UID=%d, GID=%d", hdr.Uid, hdr.Gid)
				}
			}

			t.Logf("Found file %s with UID=%d, GID=%d", hdr.Name, hdr.Uid, hdr.Gid)
		}
	}

	if foundFiles == 0 {
		t.Error("No test files found in tar")
	}
}

func getCwd() string {
	wd, _ := os.Getwd()
	return wd
}
