// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	upstreamInstanceLabel = "upstream.instance"

	derivedNamePrefix  = "i-"
	derivedNameHashLen = 10
)

// instanceServiceName returns the name of the Service that fronts an Instance.
// Service names must be DNS-1035 labels, which Instance names are not
// guaranteed to be: they may start with a digit or exceed 63 characters.
func instanceServiceName(instanceName string) string {
	if len(validation.IsDNS1035Label(instanceName)) == 0 {
		return instanceName
	}
	return derivedInstanceName(instanceName)
}

// instanceLabelValue returns the upstream.instance label value for an
// Instance. Label values are capped at 63 characters, so longer names are
// replaced by their derived form.
func instanceLabelValue(instanceName string) string {
	if len(validation.IsValidLabelValue(instanceName)) == 0 {
		return instanceName
	}
	return derivedInstanceName(instanceName)
}

// derivedInstanceName deterministically maps an Instance name to a value that
// is both a DNS-1035 label and a valid label value. The hash suffix keeps
// distinct names distinct after sanitizing and truncation.
func derivedInstanceName(instanceName string) string {
	sum := sha256.Sum256([]byte(instanceName))
	suffix := "-" + hex.EncodeToString(sum[:])[:derivedNameHashLen]

	body := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, instanceName)

	maxBody := validation.DNS1035LabelMaxLength - len(derivedNamePrefix) - len(suffix)
	if len(body) > maxBody {
		body = body[:maxBody]
	}
	body = strings.Trim(body, "-")
	if body == "" {
		return derivedNamePrefix + suffix[1:]
	}
	return derivedNamePrefix + body + suffix
}
