package traft

// TRaftServer impl

import (
	context "context"
	"encoding/json"
	fmt "fmt"
	"math/rand"
	sync "sync"
	"time"

	"github.com/openacid/low/mathext/util"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	grpc "google.golang.org/grpc"
	// "google.golang.org/protobuf/proto"
)

var (
	ErrStaleLog  = errors.New("local log is stale")
	ErrStaleTerm = errors.New("local Term is stale")
	ErrTimeout   = errors.New("timeout")
)

var (
	llg = zap.NewNop()
	lg  *zap.SugaredLogger
)

func init() {
	// if os.Getenv("CLUSTER_DEBUG") != "" {
	// }
	var err error
	llg, err = zap.NewProduction()
	if err != nil {
		panic(err)
	}
	lg = llg.Sugar()

	// initZap()
}

func initZap() {
	rawJSON := []byte(`{
	  "level": "debug",
	  "encoding": "json",
	  "outputPaths": ["stdout", "/tmp/logs"],
	  "errorOutputPaths": ["stderr"],
	  "initialFields": {"foo": "bar"},
	  "encoderConfig": {
	    "messageKey": "message",
	    "levelKey": "level",
	    "levelEncoder": "lowercase"
	  }
	}`)

	var cfg zap.Config
	if err := json.Unmarshal(rawJSON, &cfg); err != nil {
		panic(err)
	}

	var err error
	llg, err = cfg.Build()
	if err != nil {
		panic(err)
	}
	defer llg.Sync()

	llg.Info("logger construction succeeded")

	lg = llg.Sugar()

}

type TRaft struct {
	// TODO lock first
	mu sync.Mutex

	// close it to notify all goroutines to shutdown.
	shutdown chan struct{}

	// Communication channel with Loop().
	// Only Loop() modifies state of TRaft.
	// Other goroutines send an action through this channel and wait for an
	// operation reply.
	actionCh chan *action

	grpcServer *grpc.Server

	wg sync.WaitGroup

	Node
}

// init a TRaft for test, all logs are `set x=lsn`
func (tr *TRaft) initTraft(
	// proposer of the logs
	committer *LeaderId,
	// author of the logs
	author *LeaderId,
	// log seq numbers to generate.
	lsns []int64,
	nilLogs map[int64]bool,
	committed []int64,
	votedFor *LeaderId,
) {
	id := tr.Id

	tr.LogOffset, tr.Logs = buildPseudoLogs(author, lsns, nilLogs)

	tr.Status[id].Committer = committer.Clone()
	tr.Status[id].Accepted = NewTailBitmap(0, lsns...)

	if committed == nil {
		tr.Status[id].Committed = NewTailBitmap(0)
	} else {
		tr.Status[id].Committed = NewTailBitmap(0, committed...)
	}

	tr.Status[id].VotedFor = votedFor.Clone()
}

func buildPseudoLogs(
	// author of the logs
	author *LeaderId,
	// log seq numbers to generate.
	lsns []int64,
	nilLogs map[int64]bool,
) (int64, []*Record) {
	logs := make([]*Record, 0)
	if len(lsns) == 0 {
		return 0, logs
	}

	last := lsns[len(lsns)-1]
	start := lsns[0]
	for i := start; i <= last; i++ {
		logs = append(logs, &Record{})
	}

	for _, lsn := range lsns {
		if nilLogs != nil && nilLogs[lsn] {
		} else {
			logs[lsn-start] = NewRecord(
				author.Clone(),
				lsn,
				NewCmdI64("set", "x", lsn))
		}
	}
	return start, logs
}

type trRst struct {
	logStat    *LogStatus
	leaderStat *LeaderStatus
	config     *ClusterConfig

	ok bool
}

type action struct {
	act   string
	trRst *trRst
	rstCh chan *trRst
}

// Loop handles actions from other components.
func (tr *TRaft) Loop(shutdown <-chan struct{}, act <-chan *action) {
	defer tr.wg.Done()

	id := tr.Id

	for {
		select {
		case <-shutdown:
			return
		case a := <-act:
			switch a.act {
			case "logStat":
				a.rstCh <- &trRst{
					logStat: ExportLogStatus(tr.Status[id]),
				}
			case "leaderStat":
				a.rstCh <- &trRst{
					leaderStat: ExportLeaderStatus(tr.Status[id]),
				}
			case "config":
				a.rstCh <- &trRst{
					config: tr.Config.Clone(),
				}
			case "update-leaderStat":
				leadst := a.trRst.leaderStat
				tr.Status[id].VotedFor = leadst.VotedFor.Clone()
				tr.Status[id].VoteExpireAt = leadst.VoteExpireAt
				a.rstCh <- &trRst{ok: true}
			}
		}
	}
}

func query(actCh chan<- *action, act string) *trRst {
	rstCh := make(chan *trRst)
	actCh <- &action{act, nil, rstCh}
	rst := <-rstCh
	return rst
}

func update(actCh chan<- *action, act string, rst *trRst) *trRst {
	rstCh := make(chan *trRst)
	actCh <- &action{act, rst, rstCh}
	ok := <-rstCh
	return ok

}

// run forever to elect itself as leader if there is no leader in this cluster.
func (tr *TRaft) VoteLoop(shutdown <-chan struct{}, act chan<- *action) {
	defer tr.wg.Done()

	// return true if shutting down
	slp := func(sleep time.Duration) bool {
		select {
		case <-time.After(sleep):
			lg.Infow("VoteLoop woke up")
			return false
		case <-shutdown:
			return true
		}
	}

	leaderLease := int64(time.Second * 1)
	maxStaleTermSleep := time.Millisecond * 10
	heartBeatSleep := time.Millisecond * 10
	followerSleep := time.Millisecond * 10

	leadst := query(act, "leaderStat").leaderStat
	for {

		now := uSecond()

		// TODO refine this: wait until VoteExpireAt and watch for missing
		// heartbeat.
		if now < leadst.VoteExpireAt {

			if leadst.VotedFor.Id == tr.Id {
				// I am a leader
				// TODO heartbeat other replicas to keep leadership
				if slp(heartBeatSleep) {
					return
				}
			} else {
				if slp(followerSleep) {
					return
				}
			}
			leadst = query(act, "leaderStat").leaderStat
			continue
		}

		// call for a new leader!!!

		logst := query(act, "logStat").logStat
		config := query(act, "config").config

		voted, err, higher := VoteOnce(
			leadst.VotedFor,
			logst,
			config,
		)

		if voted {
			leadst.VoteExpireAt = uSecond() + leaderLease
			update(act, "update-leaderStat", &trRst{
				leaderStat: leadst,
			})

			if slp(heartBeatSleep) {
				return
			}

		} else {
			switch errors.Cause(err) {
			case ErrStaleTerm:
				if slp(time.Millisecond*5 + time.Duration(rand.Int63n(int64(maxStaleTermSleep)))) {
					return
				}
				leadst.VotedFor.Term = higher + 1
				continue
			case ErrTimeout:
				if slp(time.Millisecond * 10) {
					return
				}
				continue
			case ErrStaleLog:
				// I can not be the leader.
				// sleep a day. waiting for others to elect to be a leader.
				if slp(time.Second * 86400) {
					return
				}
				continue
			}
		}
	}
}

// returns:
// voted or not
// error: ErrStaleLog, ErrStaleTerm, ErrTimeout
// higherTerm: if seen, upgrade term and retry
func VoteOnce(
	candidate *LeaderId,
	logStatus logStat,
	config *ClusterConfig,
) (bool, error, int64) {

	// TODO vote need cluster id:
	// a stale member may try to elect on another cluster.

	id := candidate.Id

	req := &VoteReq{
		Candidate: candidate,
		Committer: logStatus.GetCommitter(),
		Accepted:  logStatus.GetAccepted(),
	}

	type voteRes struct {
		from  *ReplicaInfo
		reply *VoteReply
	}

	ch := make(chan *voteRes)

	for _, rinfo := range config.Members {
		if rinfo.Id == id {
			continue
		}

		go func(rinfo ReplicaInfo, ch chan *voteRes) {
			rpcTo(rinfo.Addr, func(cli TRaftClient, ctx context.Context) {

				lg.Infow("send", "addr", rinfo.Addr)
				reply, err := cli.Vote(ctx, req)
				lg.Infow("recv", "from", rinfo, "reply", reply, "err", err)

				_ = reply
				if err != nil {
					// TODO
					panic("wtf")
				}

				ch <- &voteRes{&rinfo, reply}
			})
		}(*rinfo, ch)
	}

	received := uint64(0)
	// I vote myself
	received |= 1 << uint(config.Members[id].Position)
	higherTerm := int64(-1)
	var logErr error
	waitingFor := len(config.Members) - 1

	for waitingFor > 0 {
		select {
		case res := <-ch:
			repl := res.reply

			lg.Infow("got res", "res", res)

			if repl.VotedFor.Equal(candidate) {
				// vote granted
				received |= 1 << uint(res.from.Position)
				if config.IsQuorum(received) {
					// TODO cancel timer
					return true, nil, -1
				}
			} else {
				rTerm := repl.VotedFor.Term
				if rTerm > candidate.Term {
					higherTerm = util.MaxI64(higherTerm, rTerm)
				}

				if CmpLogStatus(repl, logStatus) > 0 {
					// TODO cancel timer
					logErr = errors.Wrapf(ErrStaleLog,
						"local: committer:%s max-lsn:%d remote: committer:%s max-lsn:%d",
						logStatus.GetCommitter().ShortStr(),
						logStatus.GetAccepted().Len(),
						repl.Committer.ShortStr(),
						repl.Accepted.Len())
				}
			}

			waitingFor--

		case <-time.After(time.Second):
			// timeout
			// TODO cancel timer
			return false, errors.Wrapf(ErrTimeout, "voting"), higherTerm
		}
	}

	if logErr != nil {
		return false, logErr, higherTerm
	}

	err := errors.Wrapf(ErrStaleTerm, "seen a higher term:%d", higherTerm)
	return false, err, higherTerm
}

// Only a established leader should use this func.
func (tr *TRaft) AddLog(cmd *Cmd) *Record {

	// TODO lock

	me := tr.Status[tr.Id]

	if me.VotedFor.Id != tr.Id {
		panic("wtf")
	}

	lsn := tr.LogOffset + int64(len(tr.Logs))

	r := NewRecord(me.VotedFor, lsn, cmd)

	// find the first interfering record.

	var i int
	for i = len(tr.Logs) - 1; i >= 0; i-- {
		prev := tr.Logs[i]
		if r.Interfering(prev) {
			r.Overrides = prev.Overrides.Clone()
			r.Overrides.Set(lsn)
			break
		}
	}

	if i == -1 {
		// there is not a interfering record.
		r.Overrides = NewTailBitmap(0)
	}

	// all log I do not know must be executed in order.
	// Because I do not know of the intefering relations.
	r.Depends = NewTailBitmap(tr.LogOffset)

	// reduce bitmap size by removing unknown logs
	r.Overrides.Union(NewTailBitmap(tr.LogOffset & ^63))

	tr.Logs = append(tr.Logs, r)

	return r
}

func (tr *TRaft) Vote(ctx context.Context, req *VoteReq) (*VoteReply, error) {

	me := tr.Status[tr.Id]

	// A vote reply just send back a voter's status.
	// It is the candidate's responsibility to check if a voter granted it.
	repl := &VoteReply{
		VotedFor:  me.VotedFor.Clone(),
		Committer: me.Committer.Clone(),
		Accepted:  me.Accepted.Clone(),
	}

	if CmpLogStatus(req, me) < 0 {
		// I have more logs than the candidate.
		// It cant be a leader.
		return repl, nil
	}

	// candidate has the upto date logs.

	r := req.Candidate.Cmp(me.VotedFor)
	if r < 0 {
		// I've voted for other leader with higher privilege.
		// This candidate could not be a legal leader.
		// just send back enssential info to info it.
		return repl, nil
	}

	// grant vote
	me.VotedFor = req.Candidate.Clone()
	repl.VotedFor = req.Candidate.Clone()

	// send back the logs I have but the candidate does not.

	logs := make([]*Record, 0)

	start := me.Accepted.Offset
	end := me.Accepted.Len()
	for i := start; i < end; i++ {
		if me.Accepted.Get(i) != 0 && req.Accepted.Get(i) == 0 {
			logs = append(logs, tr.Logs[i-tr.LogOffset])
		}
	}

	repl.Logs = logs

	return repl, nil
}

func (tr *TRaft) Replicate(ctx context.Context, req *ReplicateReq) (*ReplicateReply, error) {

	// TODO persist stat change.
	// TODO lock

	fmt.Println("Replicate:", req)
	me := tr.Status[tr.Id]
	{
		r := me.VotedFor.Cmp(me.Committer)
		fmt.Println(me.VotedFor)
		fmt.Println(me.Committer)
		fmt.Println(r)
		if r < 0 {
			panic("wtf")
		}

	}

	repl := &ReplicateReply{
		VotedFor: me.VotedFor.Clone(),
	}

	// check leadership

	r := req.Committer.Cmp(me.VotedFor)
	if r < 0 {
		return repl, nil
	}

	if r > 0 {
		// r>0: it is a legal leader.
		// correctness will be kept.
		// follow the (literally) greatest leader!!! :DDD
		me.VotedFor = req.Committer.Clone()
	}

	// r == 0: Committer is the same as I voted for.

	if req.Committer.Cmp(me.Committer) > 0 {
		me.Committer = req.Committer.Clone()
	}

	// TODO: if a newer committer is seen, non-committed logs
	// can be sure to stale and should be cleaned.

	for _, r := range req.Logs {
		lsn := r.Seq
		for lsn-tr.LogOffset >= int64(len(tr.Logs)) {
			// fill in the gap
			tr.Logs = append(tr.Logs, &Record{})
		}
		tr.Logs[lsn-tr.LogOffset] = r

		// If a record interferes and overrides a previous log,
		// it then does not need the overrided log to be commit to commit this
		// record.
		// As if a previous log has already accepted.
		me.Accepted.Union(r.Overrides)
	}

	repl.Accepted = me.Accepted.Clone()
	repl.Committed = me.Committed.Clone()

	return repl, nil
}
