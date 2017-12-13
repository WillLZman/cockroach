// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package acceptance

// Running these tests is best done using build/teamcity-nightly-
// acceptance.sh. See instructions therein.

import (
	gosql "database/sql"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/montanaflynn/stats"
	"github.com/pkg/errors"

	"github.com/cockroachdb/cockroach/pkg/acceptance/terrafarm"
	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/server/serverpb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/duration"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

const (
	StableInterval = 3 * time.Minute
	adminPort      = base.DefaultHTTPPort
)

// Paths to cloud storage blobs that contain stores with pre-generated data.
// Please keep /docs/cloud-resources.md up-to-date if you change these.
const (
	fixtureStore1s = "store-dumps/1node-17gb-841ranges"
	fixtureStore1m = "store-dumps/1node-113gb-9595ranges"
	fixtureStore3s = "store-dumps/3nodes-17gb-841ranges"
	fixtureStore6m = "store-dumps/6nodes-67gb-9588ranges"
)

type allocatorTest struct {
	// StartNodes is the starting number of nodes this cluster will have.
	StartNodes int
	// EndNodes is the final number of nodes this cluster will have.
	EndNodes int
	// StoreFixture is the prefix of the store dump blobs that the test will
	// download from cloud storage. For example, "store-dumps/foo" indicates that
	// stores are available at "store-dumps/foo/storeN.tgz".
	StoreFixture string
	// Prefix is the prefix that will be prepended to all resources created by
	// Terraform.
	Prefix string
	// Run some schema changes during the rebalancing.
	RunSchemaChanges bool

	// start load time.
	startLoad time.Time

	f *terrafarm.Farmer
}

func (at *allocatorTest) Cleanup(t *testing.T) {
	if r := recover(); r != nil {
		t.Errorf("recovered from panic to destroy cluster: %v", r)
	}
	if at.f != nil {
		at.f.MustDestroy(t)
	}
}

func (at *allocatorTest) Run(ctx context.Context, t *testing.T) {
	at.f = MakeFarmer(t, at.Prefix, stopper)

	log.Infof(ctx, "creating cluster with %d node(s)", at.StartNodes)
	if err := at.f.Resize(at.StartNodes); err != nil {
		t.Fatal(err)
	}
	if err := CheckGossip(ctx, at.f, waitTime, HasPeers(at.StartNodes)); err != nil {
		t.Fatal(err)
	}
	at.f.Assert(ctx, t)
	log.Info(ctx, "initial cluster is up")

	// We must stop the cluster because:
	//
	// We're about to overwrite data directories.
	//
	// We don't want the cluster above and the cluster below to ever talk to
	// each other (see #7224).
	log.Info(ctx, "stopping cluster")
	for i := 0; i < at.f.NumNodes(); i++ {
		if err := at.f.Kill(ctx, i); err != nil {
			t.Fatalf("error stopping node %d: %s", i, err)
		}
	}

	storeURL := FixtureURL(at.StoreFixture)
	log.Infof(ctx, "downloading archived stores from %s in parallel", storeURL)
	errors := make(chan error, at.f.NumNodes())
	for i := 0; i < at.f.NumNodes(); i++ {
		go func(nodeNum int) {
			errors <- at.f.Exec(nodeNum,
				fmt.Sprintf("find %[1]s -type f -delete && curl -sfSL %s/store%d.tgz | tar -C %[1]s -zx",
					"/mnt/data0/cockroach-data", storeURL, nodeNum+1,
				),
			)
		}(i)
	}
	for i := 0; i < at.f.NumNodes(); i++ {
		if err := <-errors; err != nil {
			t.Fatalf("error downloading store %d: %s", i, err)
		}
	}

	log.Info(ctx, "restarting cluster with archived store(s)")
	// Ensure all nodes get --join flags on restart.
	at.f.SkipClusterInit = true
	ch := make(chan error)
	for i := 0; i < at.f.NumNodes(); i++ {
		go func(i int) { ch <- at.f.Restart(ctx, i) }(i)
	}
	for i := 0; i < at.f.NumNodes(); i++ {
		if err := <-ch; err != nil {
			t.Errorf("error restarting node %d: %s", i, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	at.f.Assert(ctx, t)

	log.Infof(ctx, "resizing cluster to %d nodes", at.EndNodes)
	if err := at.f.Resize(at.EndNodes); err != nil {
		t.Fatal(err)
	}

	if err := CheckGossip(ctx, at.f, waitTime, HasPeers(at.EndNodes)); err != nil {
		t.Fatal(err)
	}
	at.f.Assert(ctx, t)

	log.Infof(ctx, "starting load on cluster")
	at.startLoad = timeutil.Now()
	if err := at.f.StartLoad(ctx, "block_writer"); err != nil {
		t.Fatal(err)
	}
	if err := at.f.StartLoad(ctx, "photos"); err != nil {
		t.Fatal(err)
	}

	// Rebalancing is tested in all the rebalancing tests. Speed up the
	// execution of the schema change test by not waiting for rebalancing.
	if !at.RunSchemaChanges {
		log.Info(ctx, "waiting for rebalance to finish")
		if err := at.WaitForRebalance(ctx, t); err != nil {
			t.Fatal(err)
		}
	} else {
		log.Info(ctx, "running schema changes while cluster is rebalancing")
		{
			// These schema changes are over a table that is not actively
			// being updated.
			log.Info(ctx, "running schema changes over tpch.customer")
			schemaChanges := []string{
				"ALTER TABLE tpch.customer ADD COLUMN newcol INT DEFAULT 23456",
				"CREATE INDEX foo ON tpch.customer (c_name)",
			}
			if err := at.runSchemaChanges(ctx, t, schemaChanges); err != nil {
				t.Fatal(err)
			}

			// All these return the same result.
			validationQueries := []string{
				"SELECT COUNT(*) FROM tpch.customer AS OF SYSTEM TIME %s",
				"SELECT COUNT(newcol) FROM tpch.customer AS OF SYSTEM TIME %s",
				"SELECT COUNT(c_name) FROM tpch.customer@foo AS OF SYSTEM TIME %s",
			}
			if err := at.runValidationQueries(ctx, t, validationQueries, nil); err != nil {
				t.Error(err)
			}
		}

		{
			// These schema changes are run later because the above schema
			// changes run for a decent amount of time giving datablocks.blocks
			// an opportunity to get populate through the load generator. These
			// schema changes are acting upon a decent sized table that is also
			// being updated.
			log.Info(ctx, "running schema changes over datablocks.blocks")
			schemaChanges := []string{
				"ALTER TABLE datablocks.blocks ADD COLUMN created_at TIMESTAMP DEFAULT now()",
				"CREATE INDEX foo ON datablocks.blocks (block_id, created_at)",
			}
			if err := at.runSchemaChanges(ctx, t, schemaChanges); err != nil {
				t.Fatal(err)
			}

			// All these return the same result.
			validationQueries := []string{
				"SELECT COUNT(*) FROM datablocks.blocks AS OF SYSTEM TIME %s",
				"SELECT COUNT(created_at) FROM datablocks.blocks AS OF SYSTEM TIME %s",
				"SELECT COUNT(block_id) FROM datablocks.blocks@foo AS OF SYSTEM TIME %s",
			}
			// Queries to hone in on index validation problems.
			indexValidationQueries := []string{
				"SELECT COUNT(created_at) FROM datablocks.blocks@primary AS OF SYSTEM TIME %s WHERE created_at > $1 AND created_at <= $2",
				"SELECT COUNT(block_id) FROM datablocks.blocks@foo AS OF SYSTEM TIME %s WHERE created_at > $1 AND created_at <= $2",
			}
			if err := at.runValidationQueries(
				ctx, t, validationQueries, indexValidationQueries,
			); err != nil {
				t.Error(err)
			}
		}
	}

	at.f.Assert(ctx, t)
}

func (at *allocatorTest) RunAndCleanup(ctx context.Context, t *testing.T) {
	s := log.Scope(t)
	defer s.Close(t)

	defer at.Cleanup(t)
	at.Run(ctx, t)
}

func (at *allocatorTest) runSchemaChanges(
	ctx context.Context, t *testing.T, schemaChanges []string,
) error {
	db, err := gosql.Open("postgres", at.f.PGUrl(ctx, 0))
	if err != nil {
		return err
	}
	defer func() {
		_ = db.Close()
	}()

	for _, cmd := range schemaChanges {
		start := timeutil.Now()
		log.Infof(ctx, "starting schema change: %s", cmd)
		if _, err := db.Exec(cmd); err != nil {
			t.Fatalf("hit schema change error: %s, for %s, in %s", err, cmd, timeutil.Since(start))
		}
		log.Infof(ctx, "completed schema change: %s, in %s", cmd, timeutil.Since(start))
		// TODO(vivek): Monitor progress of schema changes and log progress.
	}

	return nil
}

// The validationQueries all return the same result.
func (at *allocatorTest) runValidationQueries(
	ctx context.Context, t *testing.T, validationQueries []string, indexValidationQueries []string,
) error {
	// Sleep for a bit before validating the schema changes to
	// accommodate for time differences between nodes. Some of the
	// schema change backfill transactions might use a timestamp a bit
	// into the future. This is not a problem normally because a read
	// of schema data written into the impending future gets pushed,
	// but the reads being done here are at a specific timestamp through
	// AS OF SYSTEM TIME.
	time.Sleep(5 * time.Second)
	log.Info(ctx, "run validation queries")

	db, err := gosql.Open("postgres", at.f.PGUrl(ctx, 0))
	if err != nil {
		return err
	}
	defer func() {
		_ = db.Close()
	}()

	var nowString string
	if err := db.QueryRow("SELECT cluster_logical_timestamp()").Scan(&nowString); err != nil {
		t.Fatal(err)
	}
	var nowInNanos int64
	if _, err := fmt.Sscanf(nowString, "%d", &nowInNanos); err != nil {
		t.Fatal(err)
	}
	now := timeutil.Unix(0, nowInNanos)

	// Validate the different schema changes
	var eCount int64
	for i := range validationQueries {
		var count int64
		q := fmt.Sprintf(validationQueries[i], nowString)
		if err := db.QueryRow(q).Scan(&count); err != nil {
			return err
		}
		log.Infof(ctx, "query: %s, found %d rows", q, count)
		if count == 0 {
			t.Fatalf("%s: %d rows found", q, count)
		}
		if eCount == 0 {
			eCount = count
			// Investigate index creation problems. Always run this so we know
			// it works.
			if indexValidationQueries != nil {
				sp := timeSpan{start: at.startLoad, end: now}
				if err := at.findIndexProblem(ctx, db, sp, nowString, indexValidationQueries); err != nil {
					t.Error(err)
				}
			}
		} else if count != eCount {
			t.Errorf("%s: %d rows found, expected %d rows", q, count, eCount)
		}
	}
	return nil
}

type timeSpan struct {
	start, end time.Time
}

// Check index inconsistencies over the timeSpan and return true when
// problems are seen.
func (at *allocatorTest) checkIndexOverTimeSpan(
	ctx context.Context, db *gosql.DB, s timeSpan, nowString string, indexValidationQueries []string,
) (bool, error) {
	var eCount int64
	q := fmt.Sprintf(indexValidationQueries[0], nowString)
	if err := db.QueryRow(q, s.start, s.end).Scan(&eCount); err != nil {
		return false, err
	}
	var count int64
	q = fmt.Sprintf(indexValidationQueries[1], nowString)
	if err := db.QueryRow(q, s.start, s.end).Scan(&count); err != nil {
		return false, err
	}
	log.Infof(ctx, "counts seen %d, %d, over [%s, %s]", count, eCount, s.start, s.end)
	return count != eCount, nil
}

// Keep splitting the span of time passed and log where index
// inconsistencies are seen.
func (at *allocatorTest) findIndexProblem(
	ctx context.Context, db *gosql.DB, s timeSpan, nowString string, indexValidationQueries []string,
) error {
	spans := []timeSpan{s}
	// process all the outstanding time spans.
	for len(spans) > 0 {
		s := spans[0]
		spans = spans[1:]
		// split span into two time ranges.
		leftSpan, rightSpan := s, s
		d := s.end.Sub(s.start) / 2
		if d < 50*time.Millisecond {
			log.Infof(ctx, "problem seen over [%s, %s]", s.start, s.end)
			continue
		}
		m := s.start.Add(d)
		leftSpan.end = m
		rightSpan.start = m

		leftState, err := at.checkIndexOverTimeSpan(
			ctx, db, leftSpan, nowString, indexValidationQueries)
		if err != nil {
			return err
		}
		rightState, err := at.checkIndexOverTimeSpan(
			ctx, db, rightSpan, nowString, indexValidationQueries)
		if err != nil {
			return err
		}
		if leftState {
			spans = append(spans, leftSpan)
		}
		if rightState {
			spans = append(spans, rightSpan)
		}
		if !(leftState || rightState) {
			log.Infof(ctx, "no problem seen over [%s, %s]", s.start, s.end)
		}
	}
	return nil
}

func (at *allocatorTest) stdDev() (float64, error) {
	host := at.f.Hostname(0)
	var client http.Client
	var nodesResp serverpb.NodesResponse
	url := fmt.Sprintf("http://%s:%s/_status/nodes", host, adminPort)
	if err := httputil.GetJSON(client, url, &nodesResp); err != nil {
		return 0, err
	}
	var replicaCounts stats.Float64Data
	for _, node := range nodesResp.Nodes {
		for _, ss := range node.StoreStatuses {
			replicaCounts = append(replicaCounts, ss.Metrics["replicas"])
		}
	}
	stdDev, err := stats.StdDevP(replicaCounts)
	if err != nil {
		return 0, err
	}
	return stdDev, nil
}

// printStats prints the time it took for rebalancing to finish and the final
// standard deviation of replica counts across stores.
func (at *allocatorTest) printRebalanceStats(db *gosql.DB, host string) error {
	// TODO(cuongdo): Output these in a machine-friendly way and graph.

	// Output time it took to rebalance.
	{
		var rebalanceIntervalStr string
		if err := db.QueryRow(
			`SELECT (SELECT MAX(timestamp) FROM rangelog) - (SELECT MAX(timestamp) FROM eventlog WHERE "eventType"=$1)`,
			sql.EventLogNodeJoin,
		).Scan(&rebalanceIntervalStr); err != nil {
			return err
		}
		rebalanceInterval, err := tree.ParseDInterval(rebalanceIntervalStr)
		if err != nil {
			return err
		}
		if rebalanceInterval.Duration.Compare(duration.Duration{}) < 0 {
			log.Warningf(context.Background(), "test finished, but clock moved backward")
		} else {
			log.Infof(context.Background(), "cluster took %s to rebalance", rebalanceInterval)
		}
	}

	// Output # of range events that occurred. All other things being equal,
	// larger numbers are worse and potentially indicate thrashing.
	{
		var rangeEvents int64
		q := `SELECT COUNT(*) from rangelog`
		if err := db.QueryRow(q).Scan(&rangeEvents); err != nil {
			return err
		}
		log.Infof(context.Background(), "%d range events", rangeEvents)
	}

	// Output standard deviation of the replica counts for all stores.
	stdDev, err := at.stdDev()
	if err != nil {
		return err
	}
	log.Infof(context.Background(), "stdDev(replica count) = %.2f", stdDev)

	return nil
}

type replicationStats struct {
	ElapsedSinceLastEvent duration.Duration
	EventType             string
	RangeID               int64
	StoreID               int64
	ReplicaCountStdDev    float64
}

func (s replicationStats) String() string {
	return fmt.Sprintf("last range event: %s for range %d/store %d (%s ago)",
		s.EventType, s.RangeID, s.StoreID, s.ElapsedSinceLastEvent)
}

// allocatorStats returns the duration of stability (i.e. no replication
// changes) and the standard deviation in replica counts. Only unrecoverable
// errors are returned.
func (at *allocatorTest) allocatorStats(db *gosql.DB) (s replicationStats, err error) {
	defer func() {
		if err != nil {
			s.ReplicaCountStdDev = math.MaxFloat64
		}
	}()

	eventTypes := []interface{}{
		storage.RangeLogEventType_split.String(),
		storage.RangeLogEventType_add.String(),
		storage.RangeLogEventType_remove.String(),
	}

	q := `SELECT NOW()-timestamp, "rangeID", "storeID", "eventType" FROM rangelog WHERE ` +
		`timestamp=(SELECT MAX(timestamp) FROM rangelog WHERE "eventType" IN ($1, $2, $3))`

	var elapsedStr string

	row := db.QueryRow(q, eventTypes...)
	if row == nil {
		// This should never happen, because the archived store we're starting with
		// will always have some range events.
		return replicationStats{}, errors.New("couldn't find any range events")
	}
	if err := row.Scan(&elapsedStr, &s.RangeID, &s.StoreID, &s.EventType); err != nil {
		return replicationStats{}, err
	}
	elapsedSinceLastEvent, err := tree.ParseDInterval(elapsedStr)
	if err != nil {
		return replicationStats{}, err
	}
	s.ElapsedSinceLastEvent = elapsedSinceLastEvent.Duration

	s.ReplicaCountStdDev, err = at.stdDev()
	if err != nil {
		return replicationStats{}, err
	}

	return s, nil
}

// WaitForRebalance waits until there's been no recent range adds, removes, and
// splits. We wait until the cluster is balanced or until `StableInterval`
// elapses, whichever comes first. Then, it prints stats about the rebalancing
// process. If the replica count appears unbalanced, an error is returned.
//
// This method is crude but necessary. If we were to wait until range counts
// were just about even, we'd miss potential post-rebalance thrashing.
func (at *allocatorTest) WaitForRebalance(ctx context.Context, t *testing.T) error {
	const statsInterval = 20 * time.Second

	db, err := gosql.Open("postgres", at.f.PGUrl(ctx, 0))
	if err != nil {
		return err
	}
	defer func() {
		_ = db.Close()
	}()

	var statsTimer timeutil.Timer
	var assertTimer timeutil.Timer
	defer statsTimer.Stop()
	defer assertTimer.Stop()
	statsTimer.Reset(statsInterval)
	assertTimer.Reset(0)
	for {
		select {
		case <-statsTimer.C:
			statsTimer.Read = true
			stats, err := at.allocatorStats(db)
			if err != nil {
				return err
			}

			log.Info(ctx, stats)
			stableDuration := duration.Duration{Nanos: StableInterval.Nanoseconds()}
			if stableDuration.Compare(stats.ElapsedSinceLastEvent) <= 0 {
				host := at.f.Hostname(0)
				log.Infof(context.Background(), "replica count = %f, max = %f", stats.ReplicaCountStdDev, *flagATMaxStdDev)
				if stats.ReplicaCountStdDev > *flagATMaxStdDev {
					_ = at.printRebalanceStats(db, host)
					return errors.Errorf(
						"%s elapsed without changes, but replica count standard "+
							"deviation is %.2f (>%.2f)", stats.ElapsedSinceLastEvent,
						stats.ReplicaCountStdDev, *flagATMaxStdDev)
				}
				return at.printRebalanceStats(db, host)
			}
			statsTimer.Reset(statsInterval)
		case <-assertTimer.C:
			assertTimer.Read = true
			at.f.Assert(ctx, t)
			assertTimer.Reset(time.Minute)
		case <-stopper.ShouldStop():
			return errors.New("interrupted")
		}
	}
}

// TestUpreplicate_1To3Small tests up-replication, starting with 1 node
// containing 10 GiB of data and growing to 3 nodes.
func TestUpreplicate_1To3Small(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:   1,
		EndNodes:     3,
		StoreFixture: fixtureStore1s,
		Prefix:       "uprep-1to3s",
	}
	at.RunAndCleanup(ctx, t)
}

// TestRebalance3to5Small_WithSchemaChanges tests rebalancing in
// the midst of schema changes, starting with 3 nodes (each
// containing 10 GiB of data) and growing to 5 nodes.
func TestRebalance_3To5Small_WithSchemaChanges(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:       3,
		EndNodes:         5,
		StoreFixture:     fixtureStore3s,
		Prefix:           "rebal-3to5s",
		RunSchemaChanges: true,
	}
	at.RunAndCleanup(ctx, t)
}

// TestRebalance3to5Small tests rebalancing, starting with 3 nodes (each
// containing 10 GiB of data) and growing to 5 nodes.
func TestRebalance_3To5Small(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:   3,
		EndNodes:     5,
		StoreFixture: fixtureStore3s,
		Prefix:       "rebal-3to5s",
	}
	at.RunAndCleanup(ctx, t)
}

// TestUpreplicate_1To3Medium tests up-replication, starting with 1 node
// containing 108 GiB of data and growing to 3 nodes.
func TestUpreplicate_1To3Medium(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:   1,
		EndNodes:     3,
		StoreFixture: fixtureStore1m,
		Prefix:       "uprep-1to3m",
	}
	at.RunAndCleanup(ctx, t)
}

// TestUpreplicate_1To6Medium tests up-replication (and, to a lesser extent,
// rebalancing), starting with 1 node containing 108 GiB of data and growing to
// 6 nodes.
func TestUpreplicate_1To6Medium(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:   1,
		EndNodes:     6,
		StoreFixture: fixtureStore1m,
		Prefix:       "uprep-1to6m",
	}
	at.RunAndCleanup(ctx, t)
}

// TestSteady_6Medium is useful for creating a medium-size balanced cluster
// (when used with the -tf.keep-cluster flag).
// TODO(vivek): use for tests which drop large amounts of data.
func TestSteady_6Medium(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:       6,
		EndNodes:         6,
		StoreFixture:     fixtureStore6m,
		Prefix:           "steady-6m",
		RunSchemaChanges: true,
	}
	at.RunAndCleanup(ctx, t)
}

// TestSteady_3Small tests schema changes against a 3-node cluster.
func TestSteady_3Small(t *testing.T) {
	ctx := context.Background()
	at := allocatorTest{
		StartNodes:       3,
		EndNodes:         3,
		StoreFixture:     fixtureStore3s,
		Prefix:           "steady-3s",
		RunSchemaChanges: true,
	}
	at.RunAndCleanup(ctx, t)
}
