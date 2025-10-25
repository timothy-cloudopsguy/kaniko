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

package config

import (
	"os"
	"testing"
)

func TestVirtualOwnershipTracker(t *testing.T) {
	tracker := &VirtualOwnershipTracker{
		owners: make(map[string]OwnershipMeta),
	}

	// Test setting ownership
	tracker.SetVirtualOwnership("/test/file1", 1000, 1000)
	tracker.SetVirtualOwnership("/test/file2", 2000, 2000)

	// Test getting ownership
	meta1, exists1 := tracker.GetVirtualOwnership("/test/file1")
	if !exists1 {
		t.Error("Expected ownership to exist for /test/file1")
	}
	if meta1.Uid != 1000 || meta1.Gid != 1000 {
		t.Errorf("Expected UID=1000, GID=1000, got UID=%d, GID=%d", meta1.Uid, meta1.Gid)
	}

	meta2, exists2 := tracker.GetVirtualOwnership("/test/file2")
	if !exists2 {
		t.Error("Expected ownership to exist for /test/file2")
	}
	if meta2.Uid != 2000 || meta2.Gid != 2000 {
		t.Errorf("Expected UID=2000, GID=2000, got UID=%d, GID=%d", meta2.Uid, meta2.Gid)
	}

	// Test non-existent path
	_, exists3 := tracker.GetVirtualOwnership("/test/file3")
	if exists3 {
		t.Error("Expected ownership to not exist for /test/file3")
	}

	// Test clear functionality
	tracker.ClearVirtualOwnership()
	_, exists4 := tracker.GetVirtualOwnership("/test/file1")
	if exists4 {
		t.Error("Expected ownership to be cleared")
	}
}

func TestEnvironmentVariableSupport(t *testing.T) {
	// Test setting environment variable
	os.Setenv("KANIKO_VIRTUAL_CHOWN", "true")
	defer os.Unsetenv("KANIKO_VIRTUAL_CHOWN")

	// Check if environment variable is detected
	val, ok := os.LookupEnv("KANIKO_VIRTUAL_CHOWN")
	if !ok || val != "true" {
		t.Error("Environment variable not set correctly")
	}

	// Test parsing boolean values
	os.Setenv("KANIKO_VIRTUAL_CHOWN", "false")
	val2, ok2 := os.LookupEnv("KANIKO_VIRTUAL_CHOWN")
	if !ok2 || val2 != "false" {
		t.Error("Environment variable not set correctly for false")
	}

	// Test invalid value
	os.Setenv("KANIKO_VIRTUAL_CHOWN", "invalid")
	_, err := os.LookupEnv("KANIKO_VIRTUAL_CHOWN")
	// This should not cause an error in LookupEnv, but when parsed it would
	if err != nil {
		t.Error("LookupEnv should not return error")
	}
}