//go:build e2e_full
// +build e2e_full

package e2e_full

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2EFull(t *testing.T) {
	RegisterFailHandler(Fail)
	SetDefaultEventuallyTimeout(5 * time.Minute)
	SetDefaultEventuallyPollingInterval(3 * time.Second)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting arc-detective full e2e test suite\n")
	RunSpecs(t, "Full E2E Suite")
}
