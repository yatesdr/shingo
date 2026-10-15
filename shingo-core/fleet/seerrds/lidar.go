package seerrds

import (
	"time"

	"shingocore/fleet"
)

// lidarFrameTimeout bounds one relayed laser frame. Measured 44-81 ms at both
// plants (DATA-laser-frame-gauge.md §10), 684 ms once on a cold first call;
// five seconds is far above that and still short enough that an hourly round
// over a dozen robots cannot hang.
const lidarFrameTimeout = 5 * time.Second

var _ fleet.LidarFrameReader = (*Adapter)(nil)

// ReadLidarFrames implements fleet.LidarFrameReader through RDS's
// /generalRobokitAPI relay (rds.Client.ReadLaserStats). Core never opens a
// connection to the robot.
func (a *Adapter) ReadLidarFrames(vehicleID string) ([]fleet.LidarStats, error) {
	stats, err := a.client.ReadLaserStats(vehicleID, lidarFrameTimeout)
	if err != nil {
		return nil, err
	}
	out := make([]fleet.LidarStats, 0, len(stats))
	for _, s := range stats {
		out = append(out, fleet.LidarStats{
			Lidar: s.Lidar, UseForLoc: s.UseForLoc, Beams: s.Beams, Valid: s.Valid,
			RSSIMedValid: s.RSSIMedValid, RSSIP25Valid: s.RSSIP25Valid, RSSIP75Valid: s.RSSIP75Valid,
			NearValid: s.NearValid,
		})
	}
	return out, nil
}
