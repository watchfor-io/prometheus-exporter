package exporter

import (
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"google.golang.org/protobuf/proto"
)

func parse(t *testing.T, text string) map[string]*dto.MetricFamily {
	t.Helper()
	p := expfmt.NewTextParser(model.LegacyValidation)
	mfs, err := p.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return mfs
}

func labelString(m *dto.Metric) string {
	var b strings.Builder
	for i, lp := range m.GetLabel() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(lp.GetName() + "=" + lp.GetValue())
	}
	return b.String()
}

func TestWithOrganization(t *testing.T) {
	cases := []struct {
		name  string
		input string
		org   string
		want  map[string][]string // family -> label strings of its series, in order
	}{
		{
			name:  "no labels",
			input: "# TYPE watchfor_health_score gauge\nwatchfor_health_score 82\n",
			org:   "acme",
			want:  map[string][]string{"watchfor_health_score": {"organization=acme"}},
		},
		{
			name:  "labels stay, sorted with organization",
			input: "# TYPE watchfor_monitor_checks_24h gauge\nwatchfor_monitor_checks_24h{result=\"success\",monitor_id=\"m1\"} 5\n",
			org:   "prod",
			want:  map[string][]string{"watchfor_monitor_checks_24h": {"monitor_id=m1,organization=prod,result=success"}},
		},
		{
			name:  "existing organization label becomes exported_organization",
			input: "# TYPE watchfor_organization_info gauge\nwatchfor_organization_info{organization=\"acme-inc\",plan=\"pro\"} 1\n",
			org:   "acme",
			want:  map[string][]string{"watchfor_organization_info": {"exported_organization=acme-inc,organization=acme,plan=pro"}},
		},
		{
			name:  "both organization and exported_organization present: exported wins",
			input: "# TYPE x gauge\nx{organization=\"a\",exported_organization=\"b\"} 1\n",
			org:   "c",
			want:  map[string][]string{"x": {"exported_organization=b,organization=c"}},
		},
		{
			name:  "the exporter's own namespace is dropped",
			input: "# TYPE watchfor_exporter_up gauge\nwatchfor_exporter_up 1\n# TYPE watchfor_hosts gauge\nwatchfor_hosts{state=\"online\"} 2\n",
			org:   "acme",
			want:  map[string][]string{"watchfor_hosts": {"organization=acme,state=online"}},
		},
		{
			name:  "label values with quotes, backslashes and UTF-8 survive",
			input: "# TYPE watchfor_monitor_info gauge\nwatchfor_monitor_info{monitor_id=\"m\",name=\"a \\\"b\\\" c:\\\\d — é\"} 1\n",
			org:   "Org with spaces",
			want:  map[string][]string{"watchfor_monitor_info": {`monitor_id=m,name=a "b" c:\d — é,organization=Org with spaces`}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := parse(t, tc.input)
			before := map[string]string{}
			for n, mf := range in {
				before[n] = mf.String()
			}
			out := withOrganization(in, tc.org)
			if len(out) != len(tc.want) {
				t.Fatalf("%d families, want %d", len(out), len(tc.want))
			}
			for _, mf := range out {
				want, ok := tc.want[mf.GetName()]
				if !ok {
					t.Fatalf("unexpected family %s", mf.GetName())
				}
				if len(mf.Metric) != len(want) {
					t.Fatalf("%s: %d series, want %d", mf.GetName(), len(mf.Metric), len(want))
				}
				for i, m := range mf.Metric {
					if got := labelString(m); got != want[i] {
						t.Errorf("%s series %d: labels %q, want %q", mf.GetName(), i, got, want[i])
					}
				}
			}
			for n, mf := range in {
				if mf.String() != before[n] {
					t.Errorf("input family %s was modified", n)
				}
			}
		})
	}
}

func TestMergeTypeConflict(t *testing.T) {
	a := withOrganization(parse(t, "# TYPE x gauge\nx 1\n"), "a")
	b := withOrganization(parse(t, "# TYPE x counter\nx 2\n"), "b")
	var conflicts []string
	out := merge([][]*dto.MetricFamily{a, b}, func(n string) { conflicts = append(conflicts, n) })
	if len(out) != 1 || len(out[0].Metric) != 1 || out[0].GetType() != dto.MetricType_GAUGE {
		t.Fatalf("want the first target's gauge only, got %v", out)
	}
	if len(conflicts) != 1 || conflicts[0] != "x" {
		t.Errorf("conflicts = %v, want [x]", conflicts)
	}
}

func TestMergeDoesNotAliasStoredSlices(t *testing.T) {
	a := withOrganization(parse(t, "# TYPE x gauge\nx{k=\"1\"} 1\nx{k=\"2\"} 2\n"), "a")
	stored := proto.Clone(a[0]).(*dto.MetricFamily)
	out := merge([][]*dto.MetricFamily{a}, nil)
	out[0].Metric[0], out[0].Metric[1] = out[0].Metric[1], out[0].Metric[0]
	if !proto.Equal(a[0], stored) {
		t.Error("reordering the merged output changed the stored family")
	}
}
