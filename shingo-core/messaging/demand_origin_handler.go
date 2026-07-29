package messaging

import (
	"log"

	"shingo/protocol"
	"shingocore/store"
)

// demand_origin_handler.go — seam 3's Core side.
//
// One subject, carrying the WHOLE episode row plus a monotonic revision, and
// this handler is an UPSERT rather than a state machine. That is the entire
// point of sending state instead of events: there is no "have I seen the open
// for this close" question to get wrong, because every message is complete.
//
// NN5: this handler, its protocol.CoreInboundSubjects() entry and its
// RegisterSubject call are one change. An entry without a handler is a Core
// that does not start.
//
// That assertion no longer waits for a boot to fire: it moved out of main()
// into buildSubjectRouter (cmd/shingocore/routers.go) and is now also
// TestSubjectRouter_CoversEveryInboundSubject, which fails the build in
// milliseconds. Splitting the three-part change is still wrong; it is just no
// longer discovered at a plant. Ledger item B10.

// HandleDemandOrigin applies one episode state message from Edge.
func (s *CoreDataService) HandleDemandOrigin(env *protocol.Envelope, st *protocol.DemandOriginState) {
	if st == nil || st.OriginID == "" || st.EpisodeKey == "" {
		log.Printf("core_handler: demand origin with no origin_id/episode_key from %s — dropped", env.Src.Station)
		return
	}
	station := env.Src.Station

	// ACCEPT AND LOG, never reject. An unparseable key means something on the
	// far side built one by hand instead of using the shared constructors, and
	// that is worth knowing loudly — but dropping the episode destroys the only
	// evidence of whatever produced it, and leaves Edge believing it sent
	// something Core will never show. A weird episode beats a missing one.
	if _, err := protocol.ParseEpisodeKey(st.EpisodeKey); err != nil {
		log.Printf("core_handler: UNPARSEABLE episode key %q from station=%s origin=%s: %v — stored anyway; "+
			"some mint site is not using protocol's episode-key constructors",
			st.EpisodeKey, station, st.OriginID, err)
	}

	// SUPERSEDE BEFORE INSERT, and only for an OPEN episode.
	//
	// An open arriving for a place another episode still holds is proof the
	// older one ended: Edge's demand_origins_open has episode_key as its
	// PRIMARY KEY, so it could not have minted this one while that one was
	// open. Without this the partial unique index rejects the insert and the
	// NEWER episode is the one lost.
	//
	// A CLOSE says nothing about any other episode, so it never supersedes —
	// and it needs no room in the index anyway.
	if st.ClosedAt == nil {
		n, err := s.db.SupersedeOpenEpisode(st.EpisodeKey, st.OriginID, st.OpenedAt)
		if err != nil {
			log.Printf("core_handler: supersede open episode key=%s for origin=%s: %v",
				st.EpisodeKey, st.OriginID, err)
			return
		}
		if n > 0 {
			// Worth a line rather than silence: it means a close was lost or
			// delayed in flight, which is the outbox telling us something.
			log.Printf("core_handler: episode %s opened at a place already held open — superseded %d prior episode(s) at key=%s; "+
				"the prior close was lost or is still in flight",
				st.OriginID, n, st.EpisodeKey)
		}
	}

	if err := s.db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID:              st.OriginID,
		Revision:              st.Revision,
		EpisodeKey:            st.EpisodeKey,
		Kind:                  st.Kind,
		Direction:             st.Direction,
		Trigger:               st.Trigger,
		TriggerRef:            st.TriggerRef,
		StationID:             station,
		ProcessID:             st.ProcessID,
		CoreNodeName:          st.CoreNodeName,
		PayloadCode:           st.PayloadCode,
		OpenedAt:              st.OpenedAt,
		OpenedTotal:           st.OpenedTotal,
		Threshold:             st.Threshold,
		ExpectedOrders:        st.ExpectedOrders,
		ExpectedUnknownReason: st.ExpectedUnknownReason,
		RerequestCount:        st.RerequestCount,
		Discretionary:         st.Discretionary,
		ClosedAt:              st.ClosedAt,
		CloseReason:           st.CloseReason,
		ClosedBy:              st.ClosedBy,
	}); err != nil {
		log.Printf("core_handler: upsert demand origin %s (station=%s key=%s rev=%d): %v",
			st.OriginID, station, st.EpisodeKey, st.Revision, err)
	}
}
