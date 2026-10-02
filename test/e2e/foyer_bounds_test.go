//go:build e2e

package e2e

import (
	"bufio"
	"net/http"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// metric reads one sample from krm-foyer's metrics, through the API server's service
// proxy as admin: series is the name with its labels as scraped, such as
// krm_foyer_interruptions_total{reason="BadRequest"}. A series not yet recorded is 0.
func (f *fixture) metric(series string) float64 {
	GinkgoHelper()
	out := f.kubectl("get", "--raw", "/api/v1/namespaces/krm-foyer/services/http:krm-foyer-metrics:metrics/proxy/metrics")
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), series+" "); ok {
			v, err := strconv.ParseFloat(value, 64)
			Expect(err).NotTo(HaveOccurred(), series)
			return v
		}
	}
	return 0
}

// The bounds of docs/bounds.md, and the metrics that show how close real traffic
// comes to them.
var _ = Describe("krm-foyer's bounds", Label("foyer"), func() {
	It("serves its metrics apart from the origin, and counts its interruptions there", func(ctx SpecContext) {
		series := `krm_foyer_interruptions_total{reason="BadRequest"}`
		before := fx.metric(series)
		a := signIn(ctx, alice).viaFoyer(ctx, http.MethodGet, "/api/v1//namespaces", nil, nil)
		Expect(a.Code).To(Equal(http.StatusBadRequest), "%s", a.Body)
		Expect(fx.metric(series)).To(Equal(before + 1))

		By("and nowhere on the origin")
		for _, path := range []string{"/metrics", "/k8s/metrics"} {
			a := fx.browser().do(ctx, http.MethodGet, path, nil, nil)
			Expect(string(a.Body)).NotTo(ContainSubstring("krm_foyer_"), path)
		}
	})
})
