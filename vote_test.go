package traft

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

type candStat struct {
	candidateId *LeaderId
	committer   *LeaderId
	logs        []int64
}

type voterStat struct {
	votedFor  *LeaderId
	committer *LeaderId
	author    *LeaderId
	logs      []int64
	nilLogs   map[int64]bool
	committed []int64
}

type wantVoteReply struct {
	votedFor     *LeaderId
	committer    *LeaderId
	allLogBitmap *TailBitmap
	logs         string
}

type replicateReqStat struct {
	committer *LeaderId
	logs      []int64
	nilLogs   map[int64]bool
}

type wantReplicateReply struct {
	votedFor  *LeaderId
	accepted  *TailBitmap
	committed *TailBitmap
}

type wantVoterStat struct {
	votedFor *LeaderId
	accepted *TailBitmap
	logs     string
}

func TestTRaft_Vote(t *testing.T) {

	ta := require.New(t)

	ids := []int64{1, 2, 3}
	id := int64(1)

	trafts := serveCluster(ids)
	defer func() {
		for _, s := range trafts {
			s.Stop()
		}
	}()

	t1 := trafts[0]

	testVote := func(
		cand candStat,
		voter voterStat,
	) *VoteReply {

		t1.initTraft(
			voter.committer, voter.author, voter.logs, voter.nilLogs, nil,
			voter.votedFor,
		)

		req := &VoteReq{
			Candidate: cand.candidateId,
			Committer: cand.committer,
			Accepted:  NewTailBitmap(0, cand.logs...),
		}

		var reply *VoteReply
		addr := t1.Config.Members[id].Addr

		rpcTo(addr, func(cli TRaftClient, ctx context.Context) {
			var err error
			reply, err = cli.Vote(ctx, req)
			if err != nil {
				panic("wtf")
			}
		})

		return reply
	}

	lid := NewLeaderId

	cases := []struct {
		cand  candStat
		voter voterStat
		want  wantVoteReply
	}{
		// vote granted
		{
			candStat{candidateId: lid(2, 2), committer: lid(1, id), logs: []int64{5}},
			voterStat{votedFor: lid(0, id), committer: lid(0, id), author: lid(1, id), logs: []int64{5, 6}},
			wantVoteReply{
				votedFor:     lid(2, 2),
				committer:    lid(0, id),
				allLogBitmap: NewTailBitmap(0, 5, 6),
				logs:         "[<001#001:006{set(x, 6)}-0→0>]",
			},
		},

		// vote granted
		// send back nil logs
		{
			candStat{candidateId: lid(2, 2), committer: lid(1, id), logs: []int64{5}},
			voterStat{votedFor: lid(0, id), committer: lid(0, id), author: lid(1, id), logs: []int64{5, 6, 7}, nilLogs: map[int64]bool{6: true}},
			wantVoteReply{
				votedFor:     lid(2, 2),
				committer:    lid(0, id),
				allLogBitmap: NewTailBitmap(0, 5, 6, 7),
				logs:         "[<>, <001#001:007{set(x, 7)}-0→0>]",
			},
		},

		// candidate has no upto date logs
		{
			candStat{candidateId: lid(2, 2), committer: lid(0, id), logs: []int64{5, 6}},
			voterStat{votedFor: lid(0, id), committer: lid(1, id), author: lid(1, id), logs: []int64{5, 6}},
			wantVoteReply{
				votedFor:     lid(0, id),
				committer:    lid(1, id),
				allLogBitmap: NewTailBitmap(0, 5, 6),
				logs:         "[]",
			},
		},

		// candidate has not enough logs
		// No log is sent back to candidate because it does not need to rebuild
		// full log history.
		{
			candStat{candidateId: lid(2, 2), committer: lid(1, id), logs: []int64{5}},
			voterStat{votedFor: lid(0, id), committer: lid(1, id), author: lid(1, id), logs: []int64{5, 6}},
			wantVoteReply{
				votedFor:     lid(0, id),
				committer:    lid(1, id),
				allLogBitmap: NewTailBitmap(0, 5, 6),
				logs:         "[]",
			},
		},

		// candidate has smaller term.
		// No log sent back.
		{
			candStat{candidateId: lid(2, 2), committer: lid(1, id), logs: []int64{5, 6}},
			voterStat{votedFor: lid(3, id), committer: lid(1, id), author: lid(1, id), logs: []int64{5, 6}},
			wantVoteReply{
				votedFor:     lid(3, id),
				committer:    lid(1, id),
				allLogBitmap: NewTailBitmap(0, 5, 6),
				logs:         "[]",
			},
		},

		// candidate has smaller id.
		// No log sent back.
		{
			candStat{candidateId: lid(3, id-1), committer: lid(1, id), logs: []int64{5, 6}},
			voterStat{votedFor: lid(3, id), committer: lid(1, id), author: lid(1, id), logs: []int64{5, 6}},
			wantVoteReply{
				votedFor:     lid(3, id),
				committer:    lid(1, id),
				allLogBitmap: NewTailBitmap(0, 5, 6),
				logs:         "[]",
			},
		},
	}

	for i, c := range cases {
		reply := testVote(c.cand, c.voter)

		fmt.Println(reply.String())
		fmt.Println(RecordsShortStr(reply.Logs))

		ta.Equal(
			c.want,
			wantVoteReply{
				votedFor:     reply.VotedFor,
				committer:    reply.Committer,
				allLogBitmap: reply.Accepted,
				logs:         RecordsShortStr(reply.Logs),
			},
			"%d-th: case: %+v", i+1, c)

		ta.InDelta(int64(uSecond()+leaderLease), t1.Status[id].VoteExpireAt, 1000*1000*1000)
	}
}

func TestTRaft_VoteOnce(t *testing.T) {

	ta := require.New(t)
	_ = ta

	ids := []int64{1, 2, 3}
	id1 := int64(1)
	id2 := int64(2)
	id3 := int64(3)

	_ = id1

	lid := NewLeaderId

	trafts := serveCluster(ids)
	defer func() {
		for _, s := range trafts {
			s.Stop()
		}
	}()

	t1 := trafts[0]
	t2 := trafts[1]
	t3 := trafts[2]

	t.Run("2emptyVoter/term-0", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		voted, err, higher := VoteOnce(
			lid(0, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.Nil(voted)
		ta.Equal(ErrStaleTermId, errors.Cause(err))
		ta.Equal(int64(0), higher)
	})

	t.Run("2emptyVoter/term-1", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		voted, err, higher := VoteOnce(
			lid(1, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.NotNil(voted)
		ta.Nil(err)
		ta.Equal(int64(-1), higher)
	})
	t.Run("reject-by-one/stalelog", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(2, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		voted, err, higher := VoteOnce(
			lid(1, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.NotNil(voted)
		ta.Nil(err)
		ta.Equal(int64(-1), higher)
	})
	t.Run("reject-by-one/higherTerm", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(5, id3))

		voted, err, higher := VoteOnce(
			lid(1, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.NotNil(voted)
		ta.Nil(err)
		ta.Equal(int64(-1), higher)
	})
	t.Run("reject-by-two/stalelog", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(2, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{0}, nil, nil, lid(0, id3))

		voted, err, higher := VoteOnce(
			lid(1, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.Nil(voted)
		ta.Equal(ErrStaleLog, errors.Cause(err))
		ta.Equal(int64(-1), higher)
	})
	t.Run("reject-by-two/stalelog-higherTerm", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(2, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{0}, nil, nil, lid(5, id3))

		voted, err, higher := VoteOnce(
			lid(1, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.Nil(voted)
		ta.Equal(ErrStaleLog, errors.Cause(err))
		ta.Equal(int64(5), higher)
	})
	t.Run("reject-by-two/higherTerm", func(t *testing.T) {
		ta := require.New(t)
		t2.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(3, id2))
		t3.initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(5, id3))

		voted, err, higher := VoteOnce(
			lid(1, id1),
			ExportLogStatus(t1.Status[id1]),
			t1.Config.Clone(),
		)

		ta.Nil(voted)
		ta.Equal(ErrStaleTermId, errors.Cause(err))
		ta.Equal(int64(5), higher)
	})
}

func TestTRaft_query(t *testing.T) {

	ta := require.New(t)

	ids := []int64{1}
	id1 := int64(1)
	lid := NewLeaderId

	trafts := serveCluster(ids)
	defer func() {
		for _, s := range trafts {
			s.Stop()
		}
	}()

	t1 := trafts[0]
	t1.initTraft(lid(1, 2), lid(3, 4), []int64{5}, nil, nil, lid(0, id1))

	got := query(t1.actionCh, "logStat", nil).v.(*LogStatus)
	ta.Equal("001#002", got.Committer.ShortStr())
	ta.Equal("0:20", got.Accepted.ShortStr())
}

func stopAll(ts []*TRaft) {
	for _, s := range ts {
		s.Stop()
	}
}

func readMsg3(ts []*TRaft) string {

	// n traft and a timeout
	cases := make([]reflect.SelectCase, len(ts)+1)
	for i, t := range ts {
		cases[i] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(t.MsgCh)}
	}
	cases[len(ts)] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(time.After(time.Second))}

	chosen, value, ok := reflect.Select(cases)
	// ok will be true if the channel has not been closed.
	if chosen == len(ts) {
		panic("timeout")
	}

	_ = ok

	// t := ts[chosen]
	msg := value.String()

	// var msg string
	// select {
	// case msg = <-ts[0].MsgCh:
	// case msg = <-ts[1].MsgCh:
	// case msg = <-ts[2].MsgCh:
	// case <-time.After(time.Second):
	//     panic("timeout")
	// }

	return msg
}

func waitForMsg(ts []*TRaft, msgs map[string]int) {
	for {
		msg := readMsg3(ts)
		for s, _ := range msgs {
			if strings.Contains(msg, s) {
				msgs[s]--
				lg.Infow("got-msg", "msg", msg)
			}
		}

		all0 := true
		for _, n := range msgs {
			all0 = all0 && n == 0
		}

		lg.Infow("require-msg", "msgs", msgs)

		if all0 {
			return
		}
	}
}

func TestTRaft_VoteLoop(t *testing.T) {

	ta := require.New(t)
	_ = ta

	ids := []int64{1, 2, 3}
	id1 := int64(1)
	id2 := int64(2)
	id3 := int64(3)

	lid := NewLeaderId

	t.Run("emptyVoters/candidate-1", func(t *testing.T) {
		ta := require.New(t)
		ts := serveCluster(ids)
		defer stopAll(ts)
		ts[0].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id1))
		ts[1].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		ts[2].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		go ts[0].VoteLoop()

		waitForMsg(ts, map[string]int{
			"vote-win VotedFor:<Term:1 Id:1 >": 1,
		})

		ta.Equal(lid(1, 1), ts[0].Status[id1].VotedFor)
		ta.InDelta(int64(uSecond()+leaderLease),
			ts[0].Status[id1].VoteExpireAt, 1000*1000*1000)

		ta.Equal(lid(1, 1), ts[1].Status[id2].VotedFor)
		ta.InDelta(int64(uSecond()+leaderLease),
			ts[1].Status[id2].VoteExpireAt, 1000*1000*1000)
	})

	t.Run("emptyVoters/candidate-2", func(t *testing.T) {
		ta := require.New(t)
		ts := serveCluster(ids)
		defer stopAll(ts)
		ts[0].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id1))
		ts[1].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		ts[2].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		go ts[1].VoteLoop()
		waitForMsg(ts, map[string]int{
			"vote-win VotedFor:<Term:1 Id:2 >": 1,
		})

		ta.Equal(lid(1, 2), ts[1].Status[2].VotedFor)

		ta.InDelta(int64(uSecond()+leaderLease),
			ts[1].Status[2].VoteExpireAt, 1000*1000*1000)
	})

	t.Run("emptyVoters/candidate-12", func(t *testing.T) {
		ta := require.New(t)
		_ = ta
		ts := serveCluster(ids)
		defer stopAll(ts)
		ts[0].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id1))
		ts[1].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		ts[2].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		go ts[0].VoteLoop()
		go ts[1].VoteLoop()

		// only one succ to elect.
		// In 1 second, there wont be another winning election.
		waitForMsg(ts, map[string]int{
			"vote-win VotedFor:<Term:1 Id:2 >": 1,
		})
	})

	t.Run("emptyVoters/candidate-123", func(t *testing.T) {
		ta := require.New(t)
		_ = ta
		ts := serveCluster(ids)
		defer stopAll(ts)
		ts[0].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id1))
		ts[1].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id2))
		ts[2].initTraft(lid(0, 0), lid(0, 0), []int64{}, nil, nil, lid(0, id3))

		go ts[0].VoteLoop()
		go ts[1].VoteLoop()
		go ts[2].VoteLoop()

		// only one succ to elect.
		// In 1 second, there wont be another winning election.
		waitForMsg(ts, map[string]int{
			"vote-win":  1,
			"vote-fail": 2,
		})
	})

	t.Run("id2MaxCommitter", func(t *testing.T) {
		ta := require.New(t)
		_ = ta
		ts := serveCluster(ids)
		defer stopAll(ts)
		ts[0].initTraft(lid(2, 1), lid(0, 1), []int64{2}, nil, nil, lid(0, id1))
		ts[1].initTraft(lid(3, 2), lid(0, 1), []int64{2}, nil, nil, lid(0, id2))
		ts[2].initTraft(lid(1, 3), lid(0, 1), []int64{2}, nil, nil, lid(0, id3))

		go ts[0].VoteLoop()
		go ts[1].VoteLoop()
		go ts[2].VoteLoop()

		// only one succ to elect.
		// In 1 second, there wont be another winning election.
		waitForMsg(ts, map[string]int{
			"vote-win VotedFor:<Term:1 Id:2 >": 1,
			"vote-fail":                        2,
		})
	})

	t.Run("id2MaxLog", func(t *testing.T) {
		// we need 5 replica to collect different log from 2 replica
		ta := require.New(t)
		_ = ta

		ids := []int64{0, 1, 2, 3, 4}
		ts := serveCluster(ids)
		defer stopAll(ts)

		ts[0].initTraft(lid(2, 0), lid(1, 1), []int64{0, 2}, nil, nil, lid(0, 0))
		ts[1].initTraft(lid(3, 1), lid(1, 1), []int64{0, 4}, nil, nil, lid(0, 1))
		ts[2].initTraft(lid(1, 2), lid(2, 1), []int64{0, 3}, nil, []int64{0}, lid(0, 2))
		// ts[3].initTraft(lid(1, 2), lid(1, 1), []int64{0, 2, 3}, nil, nil, lid(0, 3))
		// ts[4].initTraft(lid(1, 2), lid(1, 1), []int64{0, 2, 3}, nil, nil, lid(0, 4))

		ts[3].Stop()
		ts[4].Stop()
		ts[1].Status[1].VotedFor = NewLeaderId(3, 1)
		go ts[1].VoteLoop()

		// only one succ to elect.
		// In 1 second, there wont be another winning election.
		waitForMsg(ts, map[string]int{
			"vote-win VotedFor:<Term:4 Id:1 >": 1,
		})

		ta.Equal(
			join("[<001#001:000{set(x, 0)}-0→0>",
				"<>",
				"<001#001:002{set(x, 2)}-0→0>",
				"<002#001:003{set(x, 3)}-0→0>",
				"<001#001:004{set(x, 4)}-0→0>]"),
			RecordsShortStr(ts[1].Logs, ""),
		)

		ta.Equal(NewLeaderId(4, 1), ts[1].Status[1].Committer)
		ta.Equal(NewTailBitmap(0, 0, 2, 3, 4), ts[1].Status[1].Accepted)
		ta.Equal(NewTailBitmap(0), ts[1].Status[1].Committed)

		ta.Equal(NewLeaderId(2, 0), ts[1].Status[0].Committer)
		// using Equal to avoid comparison between nil and []int64{}
		ta.True(NewTailBitmap(0).Equal(ts[1].Status[0].Accepted))
		ta.True(NewTailBitmap(0).Equal(ts[1].Status[0].Committed))

		ta.Equal(NewLeaderId(1, 2), ts[1].Status[2].Committer)
		// reduced Accepted to Committed
		ta.Equal(NewTailBitmap(0, 0), ts[1].Status[2].Accepted)
		ta.Equal(NewTailBitmap(0, 0), ts[1].Status[2].Committed)
	})
}

func TestTRaft_Replicate(t *testing.T) {

	ta := require.New(t)

	ids := []int64{1, 2, 3}
	id := int64(1)

	trafts := serveCluster(ids)
	defer func() {
		for _, s := range trafts {
			s.Stop()
		}
	}()

	t1 := trafts[0]
	me := t1.Status[id]

	testReplicate := func(
		rreq replicateReqStat,
		voter voterStat,
	) *ReplicateReply {

		t1.initTraft(
			voter.committer, voter.author, voter.logs, voter.nilLogs, voter.committed,
			voter.votedFor,
		)

		_, logs := buildPseudoLogs(rreq.committer, rreq.logs, rreq.nilLogs)
		req := &ReplicateReq{
			Committer: rreq.committer,
			Logs:      logs,
		}

		var reply *ReplicateReply
		addr := t1.Config.Members[id].Addr

		rpcTo(addr, func(cli TRaftClient, ctx context.Context) {
			var err error
			reply, err = cli.Replicate(ctx, req)
			if err != nil {
				panic("wtf")
			}
		})

		return reply
	}

	lid := NewLeaderId
	_ = lid

	cases := []struct {
		cand      replicateReqStat
		voter     voterStat
		want      wantReplicateReply
		wantVoter wantVoterStat
	}{
		//
		// {
		//     replicateReqStat{committer: lid(1, id), logs: []int64{5}},
		//     voterStat{votedFor: lid(1, id), committer: lid(1, id), author: lid(1, id), logs: []int64{}},
		//     wantReplicateReply{
		//         votedFor:  lid(1, id),
		//         accepted:  NewTailBitmap(0, 5),
		//         committed: NewTailBitmap(0),
		//     },
		//     wantVoterStat{
		//         votedFor: lid(1, id),
		//         accepted: NewTailBitmap(0, 5),
		//         logs:     "",
		//     },
		// },
	}

	for i, c := range cases {
		reply := testReplicate(c.cand, c.voter)

		fmt.Println(reply.String())

		ta.Equal(
			c.want,
			wantReplicateReply{
				votedFor:  reply.VotedFor,
				accepted:  reply.Accepted,
				committed: reply.Committed,
			},
			"%d-th: reply: case: %+v", i+1, c)

		ta.Equal(
			c.want,
			wantVoterStat{
				votedFor: me.VotedFor,
				accepted: me.Accepted,
				logs:     RecordsShortStr(t1.Logs),
			},
			"%d-th: voter: case: %+v", i+1, c)
	}
}

func TestTRaft_AddLog(t *testing.T) {

	ta := require.New(t)

	id := int64(1)
	tr := NewTRaft(id, map[int64]string{id: "123"})
	tr.AddLog(NewCmdI64("set", "x", 1))
	// me := tr.Status[id]

	ta.Equal("[<000#001:000{set(x, 1)}-0→0>]", RecordsShortStr(tr.Logs))

	varnames := "wxyz"

	for i := 0; i < 67; i++ {
		vi := i % len(varnames)
		tr.AddLog(NewCmdI64("set", varnames[vi:vi+1], int64(i)))
	}
	l := len(tr.Logs)
	ta.Equal("<000#001:067{set(y, 66)}-0:8888888888888880:8→0>", tr.Logs[l-1].ShortStr())

	// truncate some logs, then add another 67
	// To check Overrides and Depends

	tr.LogOffset = 65
	tr.Logs = tr.Logs[65:]

	for i := 0; i < 67; i++ {
		vi := i % len(varnames)
		tr.AddLog(NewCmdI64("set", varnames[vi:vi+1], 100+int64(i)))
	}
	l = len(tr.Logs)
	ta.Equal("<000#001:134{set(y, 166)}-64:4444444444444448:44→64:1>", tr.Logs[l-1].ShortStr())

}
