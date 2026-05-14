package main

import (
	"github.com/moonrhythm/parapet/pkg/prom"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	egressBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "deploys_dropbox_egress_bytes",
		Help: "Total bytes served from dropbox per project.",
	}, []string{"project_id"})

	downloadCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "deploys_dropbox_download_count",
		Help: "Total download requests per project.",
	}, []string{"project_id"})

	uploadBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "deploys_dropbox_upload_bytes",
		Help: "Total uploaded bytes per project.",
	}, []string{"project_id"})

	uploadCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "deploys_dropbox_upload_count",
		Help: "Total upload requests per project.",
	}, []string{"project_id"})
)

func init() {
	prom.Registry().MustRegister(
		egressBytes,
		downloadCount,
		uploadBytes,
		uploadCount,
	)
}
