// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tests

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/workload/tpch"
)

func registerTPCHConcurrency(r registry.Registry) {
	const numNodes = 4

	setupCluster := func(ctx context.Context, t test.Test, c cluster.Cluster, disableTxnStatsSampling bool) {
		c.Put(ctx, t.Cockroach(), "./cockroach", c.Range(1, numNodes-1))
		c.Put(ctx, t.DeprecatedWorkload(), "./workload", c.Node(numNodes))
		c.Start(ctx, c.Range(1, numNodes-1))

		// In order to keep more things constant throughout the different test
		// runs, we disable range merges and range movements.
		conn := c.Conn(ctx, 1)
		if _, err := conn.Exec("SET CLUSTER SETTING kv.allocator.min_lease_transfer_interval = '24h';"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec("SET CLUSTER SETTING kv.range_merge.queue_enabled = false;"); err != nil {
			t.Fatal(err)
		}
		if disableTxnStatsSampling {
			if _, err := conn.Exec("SET CLUSTER SETTING sql.txn_stats.sample_rate = 0;"); err != nil {
				t.Fatal(err)
			}
		}

		if err := loadTPCHDataset(ctx, t, c, 1 /* sf */, c.NewMonitor(ctx, c.Range(1, numNodes-1)), c.Range(1, numNodes-1)); err != nil {
			t.Fatal(err)
		}
	}

	restartCluster := func(ctx context.Context, c cluster.Cluster) {
		c.Stop(ctx, c.Range(1, numNodes-1))
		c.Start(ctx, c.Range(1, numNodes-1))
	}

	// checkConcurrency returns an error if at least one node of the cluster
	// crashes when the TPCH queries are run with the specified concurrency
	// against the cluster.
	checkConcurrency := func(ctx context.Context, t test.Test, c cluster.Cluster, concurrency int) error {
		// Make sure to kill any workloads running from the previous
		// iteration.
		_ = c.RunE(ctx, c.Node(numNodes), "killall workload")

		restartCluster(ctx, c)

		// Scatter the ranges so that a poor initial placement (after loading
		// the data set) doesn't impact the results much.
		conn := c.Conn(ctx, 1)
		if _, err := conn.Exec("USE tpch;"); err != nil {
			t.Fatal(err)
		}
		scatterTables(t, conn, tpchTables)
		WaitFor3XReplication(t, conn)

		// Populate the range cache on each node.
		for node := 1; node < numNodes; node++ {
			conn = c.Conn(ctx, node)
			if _, err := conn.Exec("USE tpch;"); err != nil {
				t.Fatal(err)
			}
			for _, table := range tpchTables {
				if _, err := conn.Exec(fmt.Sprintf("SELECT count(*) FROM %s", table)); err != nil {
					t.Fatal(err)
				}
			}
		}

		m := c.NewMonitor(ctx, c.Range(1, numNodes-1))
		m.Go(func(ctx context.Context) error {
			t.Status(fmt.Sprintf("running with concurrency = %d", concurrency))
			// Run each query once on each connection.
			for queryNum := 1; queryNum <= tpch.NumQueries; queryNum++ {
				// The way --max-ops flag works is as follows: the global ops
				// counter is incremented **after** each worker completes a
				// single operation, so it is possible for all connections start
				// up, issue queries, and then the "faster" connections (those
				// for which the queries return sooner) will issue the next
				// query because the global ops counter hasn't reached the
				// --max-ops limit. Only once the limit is reached, no new
				// queries are issued, yet the workload still waits for the
				// already issued queries to complete.
				//
				// Consider the following example: we use --concurrency=3,
				// --max-ops=3, and imagine that
				//   - conn1 completes a query in 1s
				//   - conn2 completes a query in 2s
				//   - conn3 completes a query in 3s.
				// The workload will behave as follows:
				// 1. all three connections issue queries, so we have 3 queries
				//    in flight, 0 completed.
				// 2. after 1s, conn1 completes a query, increases the counter
				//    to 1 which is lower than 3, so it issues another query. We
				//    have 3 queries in flight, 1 completed.
				// 3. after 2s, conn1 and conn2 complete their queries, both
				//    increase a counter, which will eventually become 3. The
				//    connection that increased the counter first will issue
				//    another query, let's assume that conn1 was first. We have
				//    2 queries in flight, 3 completed. conn2 is closed.
				// 4. after 3s, conn1 and conn3 complete their queries and both
				//    exit. In the end a total of 5 ops were completed.
				//
				// In order to make it so that each connection executes the
				// query at least once and usually exactly once, we make the
				// --max-ops flag pretty small. We still want to give enough
				// time to the workload to spin up all connections, so we make
				// it proportional to the total concurrency.
				maxOps := concurrency / 10
				// Use very short duration for --display-every parameter so that
				// all query runs are logged.
				cmd := fmt.Sprintf(
					"./workload run tpch {pgurl:1-%d} --display-every=1ns --tolerate-errors "+
						"--count-errors --queries=%d --concurrency=%d --max-ops=%d",
					numNodes-1, queryNum, concurrency, maxOps,
				)
				if err := c.RunE(ctx, c.Node(numNodes), cmd); err != nil {
					return err
				}
			}
			return nil
		})
		return m.WaitE()
	}

	runTPCHConcurrency := func(ctx context.Context, t test.Test, c cluster.Cluster, disableTxnStatsSampling bool) {
		setupCluster(ctx, t, c, disableTxnStatsSampling)
		// TODO(yuzefovich): once we have a good grasp on the expected value for
		// max supported concurrency, we should use search.Searcher instead of
		// the binary search here. Additionally, we should introduce an
		// additional step to ensure that some kind of lower bound for the
		// supported concurrency is always sustained and fail the test if it
		// isn't.
		minConcurrency, maxConcurrency := 32, 192
		// Run the binary search to find the largest concurrency that doesn't
		// crash a node in the cluster. The current range is represented by
		// [minConcurrency, maxConcurrency).
		for minConcurrency < maxConcurrency-1 {
			concurrency := (minConcurrency + maxConcurrency) / 2
			if err := checkConcurrency(ctx, t, c, concurrency); err != nil {
				maxConcurrency = concurrency
			} else {
				minConcurrency = concurrency
			}
		}
		// Restart the cluster so that if any nodes crashed in the last
		// iteration, it doesn't fail the test.
		restartCluster(ctx, c)
		t.Status(fmt.Sprintf("max supported concurrency is %d", minConcurrency))
		// Write the concurrency number into the stats.json file to be used by
		// the roachperf.
		c.Run(ctx, c.Node(numNodes), "mkdir", t.PerfArtifactsDir())
		cmd := fmt.Sprintf(
			`echo '{ "max_concurrency": %d }' > %s/stats.json`,
			minConcurrency, t.PerfArtifactsDir(),
		)
		c.Run(ctx, c.Node(numNodes), cmd)
	}

	for _, disableTxnStatsSampling := range []bool{false, true} {
		name := "tpch_concurrency"
		if disableTxnStatsSampling {
			name += "/no_sampling"
		}
		r.Add(registry.TestSpec{
			Name:    name,
			Owner:   registry.OwnerSQLQueries,
			Cluster: r.MakeClusterSpec(numNodes),
			Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
				runTPCHConcurrency(ctx, t, c, disableTxnStatsSampling)
			},
			// By default, the timeout is 10 hours which might not be sufficient
			// given that a single iteration of checkConcurrency might take on
			// the order of one hour, so in order to let each test run to
			// complete we'll give it 18 hours. Successful runs typically take
			// a lot less, around six hours.
			Timeout: 18 * time.Hour,
		})
	}
}