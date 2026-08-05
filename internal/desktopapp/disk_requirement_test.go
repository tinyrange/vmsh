package desktopapp

import (
	"testing"

	"j5.nz/cc/client"
)

func TestCompressedImageDiskRequirementReflectsStoredRepresentation(t *testing.T) {
	plan := client.ImagePullPlan{
		BytesTotal:      2_688_861_321,
		BytesToDownload: 2_688_861_321,
	}
	required := estimatedDiskRequirement(plan, true)
	if required <= plan.BytesToDownload {
		t.Fatalf("required bytes = %d, want room beyond the compressed blobs", required)
	}
	if required >= plan.BytesToDownload+plan.BytesToDownload/2 {
		t.Fatalf("required bytes = %d, unexpectedly treats compressed blobs as expanded copies", required)
	}
}

func TestCachedCompressedImageOnlyRequiresRuntimeHeadroom(t *testing.T) {
	const want = int64(128 << 20)
	if got := estimatedDiskRequirement(client.ImagePullPlan{}, true); got != want {
		t.Fatalf("cached image requirement = %d, want %d", got, want)
	}
}
