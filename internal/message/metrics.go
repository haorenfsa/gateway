// Copyright Envoy Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package message

import "github.com/envoyproxy/gateway/internal/metrics"

var (
	watchableDepth = metrics.NewGauge(
		"watchable_depth",
		"Current depth of watchable queue.",
	)

	watchableSubscribeDurationSeconds = metrics.NewHistogram(
		"watchable_subscribe_duration_seconds",
		"How long in seconds a subscribed watchable queue is handled.",
		[]float64{0.01, 0.03, 0.1, 0.3, 0.5, 1, 3, 5, 10},
	)

	watchableSubscribeTotal = metrics.NewCounter(
		"watchable_subscribe_total",
		"Total number of subscribed watchable queue.",
	)

	runnerLabel  = metrics.NewLabel("runner")
	messageLabel = metrics.NewLabel("message")
)
