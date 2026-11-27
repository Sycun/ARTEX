package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// FindingProofCheck is the server-side verification result for prove_goal when
// the evidence node is a finding.
type FindingProofCheck struct {
	Proven            bool
	FindingID         int64
	MatchedTrafficIDs []string
	Reason            string
}

// VerifyFindingProofForNode reports whether the finding for nodeID has at least
// one bound traffic reference whose host matches a linked asset and whose
// capture the caller can still see. A nil checker or a lookup error fails closed.
func (s *ExplorationStore) VerifyFindingProofForNode(
	ctx context.Context,
	nodeID int64,
	exchangeExists func(ctx context.Context, trafficID string) (bool, error),
) (*FindingProofCheck, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("exploration store is unavailable")
	}
	var (
		findingID int64
		assetRaw  []byte
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, COALESCE(asset_ids,'[]'::jsonb) FROM findings WHERE node_id=$1`, nodeID).Scan(&findingID, &assetRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return &FindingProofCheck{Reason: "no finding record or bound traffic evidence exists for this node"}, nil
	}
	if err != nil {
		return nil, err
	}
	var assetIDs []int64
	_ = json.Unmarshal(assetRaw, &assetIDs)
	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(s.source_traffic_id,''), COALESCE(s.url,'')
FROM finding_traffic_bindings b
JOIN traffic_evidence_snapshots s ON s.id = b.snapshot_id
WHERE b.finding_id = $1
ORDER BY b.position, b.id`, findingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type boundSnap struct {
		trafficID string
		rawURL    string
	}
	var snaps []boundSnap
	for rows.Next() {
		var bs boundSnap
		if err := rows.Scan(&bs.trafficID, &bs.rawURL); err != nil {
			return nil, err
		}
		snaps = append(snaps, bs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return &FindingProofCheck{FindingID: findingID, Reason: "finding has no bound traffic_ref"}, nil
	}
	if len(assetIDs) == 0 {
		return &FindingProofCheck{FindingID: findingID, Reason: "finding has no linked asset (asset_ids is empty), so its bound traffic cannot be matched to a target"}, nil
	}
	assets, err := s.db.Assets().GetByIDs(assetIDs)
	if err != nil {
		return nil, fmt.Errorf("load finding assets: %w", err)
	}
	var assetHosts, assetRootDomains []string
	addHost := func(h string) {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h != "" {
			assetHosts = append(assetHosts, h)
		}
	}
	for _, a := range assets {
		if a.Type == "root_domain" && strings.TrimSpace(a.Domain) != "" {
			assetRootDomains = append(assetRootDomains, strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a.Domain), ".")))
		}
		addHost(a.Domain)
		addHost(a.IP)
		for _, bd := range a.BoundDomains {
			addHost(bd)
		}
		if a.URL != "" {
			if u, err := url.Parse(a.URL); err == nil {
				addHost(u.Hostname())
			}
		}
	}
	if len(assetHosts) == 0 && len(assetRootDomains) == 0 {
		return &FindingProofCheck{FindingID: findingID, Reason: "finding's linked assets have no host to match its bound traffic against"}, nil
	}
	hostMatches := func(snapHost string) bool {
		for _, h := range assetHosts {
			if snapHost == h {
				return true
			}
		}
		for _, rd := range assetRootDomains {
			if snapHost == rd || strings.HasSuffix(snapHost, "."+rd) {
				return true
			}
		}
		return false
	}
	var matched []string
	missingInCapture, hostMismatch := false, false
	for _, sn := range snaps {
		tid := strings.TrimSpace(sn.trafficID)
		if tid == "" || exchangeExists == nil {
			if exchangeExists == nil {
				return nil, errors.New("traffic capture verification is unavailable")
			}
			missingInCapture = true
			continue
		}
		ok, err := exchangeExists(ctx, tid)
		if err != nil {
			return nil, fmt.Errorf("traffic capture lookup failed for %q: %w", tid, err)
		}
		if !ok {
			missingInCapture = true
			continue
		}
		u, err := url.Parse(strings.TrimSpace(sn.rawURL))
		if err != nil {
			hostMismatch = true
			continue
		}
		snapHost := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
		if snapHost == "" || !hostMatches(snapHost) {
			hostMismatch = true
			continue
		}
		matched = append(matched, tid)
	}
	if len(matched) > 0 {
		return &FindingProofCheck{Proven: true, FindingID: findingID, MatchedTrafficIDs: matched}, nil
	}
	reason := "finding has no verified bound traffic_ref"
	if hostMismatch && !missingInCapture {
		reason = "bound traffic host does not match any asset of the finding"
	} else if missingInCapture {
		reason = "bound traffic_ref does not exist in this task's capture"
	}
	return &FindingProofCheck{FindingID: findingID, Reason: reason}, nil
}
