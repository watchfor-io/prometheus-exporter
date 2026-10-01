package exporter

import (
	"sort"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

const (
	// OrganizationLabel is added to every series a target returns.
	OrganizationLabel = "organization"
	// ExportedOrganizationLabel keeps a label the endpoint already called
	// organization (watchfor_organization_info carries the slug there), the
	// same renaming Prometheus applies when a target label collides.
	ExportedOrganizationLabel = "exported_organization"
	selfPrefix                = "watchfor_exporter_"
)

// withOrganization returns copies of the parsed families, sorted by name,
// with organization=org on every series. The input is not modified. Families
// in the exporter's own namespace are dropped so a response can never shadow
// the exporter's health metrics.
func withOrganization(families map[string]*dto.MetricFamily, org string) []*dto.MetricFamily {
	out := make([]*dto.MetricFamily, 0, len(families))
	for name, mf := range families {
		if strings.HasPrefix(name, selfPrefix) {
			continue
		}
		cp := &dto.MetricFamily{
			Name:   mf.Name,
			Help:   mf.Help,
			Type:   mf.Type,
			Unit:   mf.Unit,
			Metric: make([]*dto.Metric, 0, len(mf.Metric)),
		}
		for _, m := range mf.Metric {
			cp.Metric = append(cp.Metric, relabel(m, org))
		}
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

func relabel(m *dto.Metric, org string) *dto.Metric {
	cp := proto.Clone(m).(*dto.Metric)
	labels := make([]*dto.LabelPair, 0, len(cp.Label)+1)
	hasExported := false
	for _, lp := range cp.Label {
		if lp.GetName() == ExportedOrganizationLabel {
			hasExported = true
		}
	}
	for _, lp := range cp.Label {
		if lp.GetName() == OrganizationLabel {
			if hasExported {
				continue
			}
			lp.Name = proto.String(ExportedOrganizationLabel)
		}
		labels = append(labels, lp)
	}
	labels = append(labels, &dto.LabelPair{Name: proto.String(OrganizationLabel), Value: proto.String(org)})
	// Sorted label pairs: client_golang sorts unsorted ones in place while
	// gathering, which would race with a concurrent scrape.
	sort.Slice(labels, func(i, j int) bool { return labels[i].GetName() < labels[j].GetName() })
	cp.Label = labels
	return cp
}

// merge combines the families of several targets into one list sorted by
// name. HELP and TYPE come from the first target that has the family; a
// family whose type differs in a later target is skipped for that target and
// reported through conflict.
func merge(perTarget [][]*dto.MetricFamily, conflict func(name string)) []*dto.MetricFamily {
	byName := map[string]*dto.MetricFamily{}
	for _, families := range perTarget {
		for _, mf := range families {
			existing, ok := byName[mf.GetName()]
			if !ok {
				existing = &dto.MetricFamily{Name: mf.Name, Help: mf.Help, Type: mf.Type, Unit: mf.Unit}
				byName[mf.GetName()] = existing
			}
			if existing.GetType() != mf.GetType() {
				if conflict != nil {
					conflict(mf.GetName())
				}
				continue
			}
			existing.Metric = append(existing.Metric, mf.Metric...)
		}
	}
	out := make([]*dto.MetricFamily, 0, len(byName))
	for _, mf := range byName {
		out = append(out, mf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}
