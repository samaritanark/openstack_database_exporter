package cinder

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vexxhost/openstack_database_exporter/internal/util"
	cinderdb "github.com/vexxhost/openstackdb/cinder/db"
)

var (
	volumesUpDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, Subsystem, "up"),
		"up",
		nil,
		nil,
	)

	volumeGbDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, Subsystem, "volume_gb"),
		"volume_gb",
		[]string{
			"id",
			"name",
			"status",
			"availability_zone",
			"bootable",
			"tenant_id",
			"user_id",
			"volume_type",
			"server_id",
		},
		nil,
	)

	volumeStatusDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, Subsystem, "volume_status"),
		"volume_status",
		[]string{
			"id",
			"name",
			"status",
			"bootable",
			"tenant_id",
			"size",
			"volume_type",
			"server_id",
		},
		nil,
	)

	volumeStatusCounterDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, Subsystem, "volume_status_counter"),
		"volume_status_counter",
		[]string{
			"status",
		},
		nil,
	)

	volumesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(Namespace, Subsystem, "volumes"),
		"volumes",
		nil,
		nil,
	)
)

type VolumesCollector struct {
	db      *sql.DB
	queries *cinderdb.Queries
	logger  *slog.Logger
}

func NewVolumesCollector(db *sql.DB, logger *slog.Logger) *VolumesCollector {
	return &VolumesCollector{
		db:      db,
		queries: cinderdb.New(db),
		logger: logger.With(
			"namespace", Namespace,
			"subsystem", Subsystem,
			"collector", "volumes",
		),
	}
}

func (c *VolumesCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- volumesDesc
	ch <- volumeGbDesc
	ch <- volumeStatusDesc
	ch <- volumeStatusCounterDesc
	ch <- volumesUpDesc
}

func (c *VolumesCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()

	volumes, err := c.queries.VolumeGetAllWithAttachments(ctx)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(volumesUpDesc, prometheus.GaugeValue, 0)

		c.logger.Error("failed to query", "error", err)
		return
	}

	// The attachment join can return multiple rows per volume. Keep one volume
	// for capacity and counts, and retain every distinct nonempty server ID.
	// All other projected fields come from the volume and its volume type.
	byID := make(map[string]cinderdb.VolumeGetAllWithAttachmentsRow, len(volumes))
	servers := make(map[string]map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		byID[volume.ID] = volume
		if servers[volume.ID] == nil {
			servers[volume.ID] = make(map[string]struct{})
		}
		if volume.ServerID.String != "" {
			servers[volume.ID][volume.ServerID.String] = struct{}{}
		}
	}

	volume_status_counter := map[string]int{
		"creating":          0,
		"available":         0,
		"reserved":          0,
		"attaching":         0,
		"detaching":         0,
		"in-use":            0,
		"maintenance":       0,
		"deleting":          0,
		"awaiting-transfer": 0,
		"error":             0,
		"error_deleting":    0,
		"backing-up":        0,
		"restoring-backup":  0,
		"error_backing-up":  0,
		"error_restoring":   0,
		"error_extending":   0,
		"downloading":       0,
		"uploading":         0,
		"retyping":          0,
		"extending":         0,
	}

	for id, volume := range byID {
		serverIDs := make([]string, 0, len(servers[id]))
		for serverID := range servers[id] {
			serverIDs = append(serverIDs, serverID)
		}
		slices.Sort(serverIDs)
		serverID := strings.Join(serverIDs, ",")

		volume_status_counter[volume.Status.String]++

		ch <- prometheus.MustNewConstMetric(
			volumeGbDesc,
			prometheus.GaugeValue,
			float64(volume.Size.Int32),
			volume.ID,
			volume.Name.String,
			volume.Status.String,
			volume.AvailabilityZone.String,
			strconv.FormatBool(volume.Bootable.Bool),
			volume.ProjectID.String,
			volume.UserID.String,
			volume.VolumeType.String,
			serverID,
		)

		ch <- prometheus.MustNewConstMetric(
			volumeStatusDesc,
			prometheus.GaugeValue,
			util.StatusToValue(volume.Status.String, []string{
				"creating",
				"available",
				"reserved",
				"attaching",
				"detaching",
				"in-use",
				"maintenance",
				"deleting",
				"awaiting-transfer",
				"error",
				"error_deleting",
				"backing-up",
				"restoring-backup",
				"error_backing-up",
				"error_restoring",
				"error_extending",
				"downloading",
				"uploading",
				"retyping",
				"extending",
			}),
			volume.ID,
			volume.Name.String,
			volume.Status.String,
			strconv.FormatBool(volume.Bootable.Bool),
			volume.ProjectID.String,
			strconv.Itoa(int(volume.Size.Int32)),
			volume.VolumeType.String,
			serverID,
		)
	}

	for status, count := range volume_status_counter {
		ch <- prometheus.MustNewConstMetric(
			volumeStatusCounterDesc,
			prometheus.GaugeValue,
			float64(count),
			status,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		volumesDesc,
		prometheus.GaugeValue,
		float64(len(byID)),
	)

	ch <- prometheus.MustNewConstMetric(volumesUpDesc, prometheus.GaugeValue, 1)
}
