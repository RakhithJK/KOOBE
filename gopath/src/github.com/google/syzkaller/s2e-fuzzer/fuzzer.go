// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"flag"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/syzkaller/pkg/hash"
	"github.com/google/syzkaller/pkg/host"
	"github.com/google/syzkaller/pkg/ipc"
	"github.com/google/syzkaller/pkg/ipc/ipcconfig"
	"github.com/google/syzkaller/pkg/log"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/rpctype"
	"github.com/google/syzkaller/pkg/signal"
	"github.com/google/syzkaller/prog"
	_ "github.com/google/syzkaller/sys"
)

const (
	SEED_COVERAGE = 0
	SEED_OFFSET   = 1
	SEED_VALUE    = 2
	SEED_LENGTH   = 3

	CAP_OFFSET = 1
	CAP_VALUES = 2
	CAP_SPOT   = 4
	CAP_LENGTH = 8
)

type Fuzzer struct {
	name        string
	outputType  OutputType
	config      *ipc.Config
	execOpts    *ipc.ExecOpts
	procs       []*Proc
	gate        *ipc.Gate
	workQueue   *WorkQueue
	needPoll    chan struct{}
	choiceTable *prog.ChoiceTable
	stats       [StatCount]uint64
	manager     *rpctype.RPCClient
	target      *prog.Target

	faultInjectionEnabled    bool
	comparisonTracingEnabled bool

	corpusMu     sync.RWMutex
	corpus       [4][]*prog.Prog
	corpusHashes map[hash.Sig]struct{}
	rnd          *rand.Rand

	signalMu     sync.RWMutex
	corpusSignal signal.Signal // signal of inputs in corpus
	maxSignal    signal.Signal // max signal ever observed including flakes
	newSignal    signal.Signal // diff of maxSignal since last sync with master

	logMu sync.Mutex
}

type Stat int

const (
	StatGenerate Stat = iota
	StatFuzz
	StatCandidate
	StatTriage
	StatMinimize
	StatSmash
	StatHint
	StatSeed
	StatCount
)

var statNames = [StatCount]string{
	StatGenerate:  "exec gen",
	StatFuzz:      "exec fuzz",
	StatCandidate: "exec candidate",
	StatTriage:    "exec triage",
	StatMinimize:  "exec minimize",
	StatSmash:     "exec smash",
	StatHint:      "exec hints",
	StatSeed:      "exec seeds",
}

type OutputType int

const (
	OutputNone OutputType = iota
	OutputStdout
	OutputDmesg
	OutputFile
)

func main() {
	debug.SetGCPercent(50)

	var (
		flagName    = flag.String("name", "test", "unique name for manager")
		flagOS      = flag.String("os", runtime.GOOS, "target OS")
		flagArch    = flag.String("arch", runtime.GOARCH, "target arch")
		flagManager = flag.String("manager", "", "manager rpc address")
		flagProcs   = flag.Int("procs", 1, "number of parallel test processes")
		flagOutput  = flag.String("output", "stdout", "write programs to none/stdout/dmesg/file")
		flagPprof   = flag.String("pprof", "", "address to serve pprof profiles")
		flagTest    = flag.Bool("test", false, "enable image testing mode")      // used by syz-ci
		flagRunTest = flag.Bool("runtest", false, "enable program testing mode") // used by pkg/runtest
	)
	flag.Parse()
	outputType := parseOutputType(*flagOutput)
	log.Logf(0, "fuzzer started")

	target, err := prog.GetTarget(*flagOS, *flagArch)
	if err != nil {
		log.Fatalf("%v", err)
	}

	config, execOpts, err := ipcconfig.Default(target)
	if err != nil {
		log.Fatalf("failed to create default ipc config: %v", err)
	}
	sandbox := ipc.FlagsToSandbox(config.Flags)
	shutdown := make(chan struct{})
	osutil.HandleInterrupts(shutdown)
	go func() {
		// Handles graceful preemption on GCE.
		<-shutdown
		log.Logf(0, "SYZ-FUZZER: PREEMPTED")
		os.Exit(1)
	}()

	checkArgs := &checkArgs{
		target:      target,
		sandbox:     sandbox,
		ipcConfig:   config,
		ipcExecOpts: execOpts,
	}
	if *flagTest {
		testImage(*flagManager, checkArgs)
		return
	}

	if *flagPprof != "" {
		go func() {
			err := http.ListenAndServe(*flagPprof, nil)
			log.Fatalf("failed to serve pprof profiles: %v", err)
		}()
	} else {
		runtime.MemProfileRate = 0
	}

	log.Logf(0, "dialing manager at %v", *flagManager)
	manager, err := rpctype.NewRPCClient(*flagManager)
	if err != nil {
		log.Fatalf("failed to connect to manager: %v ", err)
	}
	a := &rpctype.ConnectArgs{Name: *flagName}
	r := &rpctype.ConnectRes{}
	if err := manager.Call("Manager.Connect", a, r); err != nil {
		log.Fatalf("failed to connect to manager: %v ", err)
	}
	if r.CheckResult == nil {
		checkArgs.gitRevision = r.GitRevision
		checkArgs.targetRevision = r.TargetRevision
		checkArgs.enabledCalls = r.EnabledCalls
		checkArgs.allSandboxes = r.AllSandboxes
		r.CheckResult, err = checkMachine(checkArgs)
		if err != nil {
			r.CheckResult = &rpctype.CheckArgs{
				Error: err.Error(),
			}
		}
		r.CheckResult.Name = *flagName
		if err := manager.Call("Manager.Check", r.CheckResult, nil); err != nil {
			log.Fatalf("Manager.Check call failed: %v", err)
		}
		if r.CheckResult.Error != "" {
			log.Fatalf("%v", r.CheckResult.Error)
		}
	}
	log.Logf(0, "syscalls: %v", len(r.CheckResult.EnabledCalls[sandbox]))
	for _, feat := range r.CheckResult.Features {
		log.Logf(0, "%v: %v", feat.Name, feat.Reason)
	}
	periodicCallback, err := host.Setup(target, r.CheckResult.Features)
	if err != nil {
		log.Fatalf("BUG: %v", err)
	}
	var gateCallback func()
	if periodicCallback != nil {
		gateCallback = func() { periodicCallback(r.MemoryLeakFrames) }
	}
	if r.CheckResult.Features[host.FeatureExtraCoverage].Enabled {
		config.Flags |= ipc.FlagExtraCover
	}
	if r.CheckResult.Features[host.FeatureFaultInjection].Enabled {
		config.Flags |= ipc.FlagEnableFault
	}
	if r.CheckResult.Features[host.FeatureNetworkInjection].Enabled {
		config.Flags |= ipc.FlagEnableTun
	}
	if r.CheckResult.Features[host.FeatureNetworkDevices].Enabled {
		config.Flags |= ipc.FlagEnableNetDev
	}
	config.Flags |= ipc.FlagEnableNetReset
	config.Flags |= ipc.FlagEnableCgroups
	config.Flags |= ipc.FlagEnableBinfmtMisc
	config.Flags |= ipc.FlagEnableCloseFds

	if *flagRunTest {
		runTest(target, manager, *flagName, config.Executor)
		return
	}

	needPoll := make(chan struct{}, 1)
	needPoll <- struct{}{}
	fuzzer := &Fuzzer{
		name:                     *flagName,
		outputType:               outputType,
		config:                   config,
		execOpts:                 execOpts,
		gate:                     ipc.NewGate(2**flagProcs, gateCallback),
		workQueue:                newWorkQueue(*flagProcs, needPoll),
		needPoll:                 needPoll,
		manager:                  manager,
		target:                   target,
		faultInjectionEnabled:    r.CheckResult.Features[host.FeatureFaultInjection].Enabled,
		comparisonTracingEnabled: r.CheckResult.Features[host.FeatureComparisons].Enabled,
		corpusHashes:             make(map[hash.Sig]struct{}),
		rnd:                      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	for i := 0; fuzzer.poll(i == 0, nil); i++ {
	}
	calls := make(map[*prog.Syscall]bool)
	for _, id := range r.CheckResult.EnabledCalls[sandbox] {
		calls[target.Syscalls[id]] = true
	}
	prios := target.CalculatePriorities(fuzzer.corpus[SEED_COVERAGE])
	fuzzer.choiceTable = target.BuildChoiceTable(prios, calls)

	for pid := 0; pid < *flagProcs; pid++ {
		proc, err := newProc(fuzzer, pid)
		if err != nil {
			log.Fatalf("failed to create proc: %v", err)
		}
		fuzzer.procs = append(fuzzer.procs, proc)
		go proc.loop()
	}

	fuzzer.pollLoop()
}

func (fuzzer *Fuzzer) pollLoop() {
	var execTotal uint64
	var lastPoll time.Time
	var lastPrint time.Time
	ticker := time.NewTicker(3 * time.Second).C
	for {
		poll := false
		select {
		case <-ticker:
		case <-fuzzer.needPoll:
			poll = true
		}
		if fuzzer.outputType != OutputStdout && time.Since(lastPrint) > 10*time.Second {
			// Keep-alive for manager.
			log.Logf(0, "alive, executed %v", execTotal)
			lastPrint = time.Now()
		}
		if poll || time.Since(lastPoll) > 10*time.Second {
			needCandidates := fuzzer.workQueue.wantCandidates()
			if poll && !needCandidates {
				continue
			}
			stats := make(map[string]uint64)
			for _, proc := range fuzzer.procs {
				stats["exec total"] += atomic.SwapUint64(&proc.env.StatExecs, 0)
				stats["executor restarts"] += atomic.SwapUint64(&proc.env.StatRestarts, 0)
			}
			for stat := Stat(0); stat < StatCount; stat++ {
				v := atomic.SwapUint64(&fuzzer.stats[stat], 0)
				stats[statNames[stat]] = v
				execTotal += v
			}
			if !fuzzer.poll(needCandidates, stats) {
				lastPoll = time.Now()
			}
		}
	}
}

func (fuzzer *Fuzzer) poll(needCandidates bool, stats map[string]uint64) bool {
	a := &rpctype.PollArgs{
		Name:           fuzzer.name,
		NeedCandidates: needCandidates,
		MaxSignal:      fuzzer.grabNewSignal().Serialize(),
		Stats:          stats,
	}
	r := &rpctype.PollRes{}
	if err := fuzzer.manager.Call("Manager.Poll", a, r); err != nil {
		log.Fatalf("Manager.Poll call failed: %v", err)
	}
	maxSignal := r.MaxSignal.Deserialize()
	log.Logf(1, "poll: candidates=%v inputs=%v signal=%v",
		len(r.Candidates), len(r.NewInputs), maxSignal.Len())
	fuzzer.addMaxSignal(maxSignal)
	for _, inp := range r.NewInputs {
		fuzzer.addInputFromAnotherFuzzer(inp)
	}
	for _, candidate := range r.Candidates {
		p, err := fuzzer.target.Deserialize(candidate.Prog, prog.NonStrict)
		if err != nil {
			log.Fatalf("failed to parse program from manager: %v", err)
		}
		flags := ProgCandidate
		if candidate.Minimized {
			flags |= ProgMinimized
		}
		if candidate.Smashed {
			flags |= ProgSmashed
		}
		fuzzer.workQueue.enqueue(&WorkCandidate{
			p:     p,
			flags: flags,
		})

		// addInputToCorpus
		// sig := hash.Hash(candidate.Prog)
		// fuzzer.addS2EInputToCorpus(p, signal.Signal{}, sig)
	}
	return len(r.NewInputs) != 0 || len(r.Candidates) != 0 || maxSignal.Len() != 0
}

func (fuzzer *Fuzzer) sendS2EInputToManager(inp rpctype.S2EInput) {
	a := &rpctype.NewS2EInputArgs{
		Name:     fuzzer.name,
		S2EInput: inp,
	}
	if err := fuzzer.manager.Call("Manager.NewS2EInput", a, nil); err != nil {
		log.Fatalf("Manager.NewS2EInput call failed: %v", err)
	}
}

func (fuzzer *Fuzzer) sendInputToManager(inp rpctype.RPCInput) {
	// a := &rpctype.NewInputArgs{
	// 	Name:     fuzzer.name,
	// 	RPCInput: inp,
	// }
	// if err := fuzzer.manager.Call("Manager.NewInput", a, nil); err != nil {
	// 	log.Fatalf("Manager.NewInput call failed: %v", err)
	// }
}

func (fuzzer *Fuzzer) addInputFromAnotherFuzzer(inp rpctype.RPCInput) {
	p, err := fuzzer.target.Deserialize(inp.Prog, prog.NonStrict)
	if err != nil {
		log.Fatalf("failed to deserialize prog from another fuzzer: %v", err)
	}
	sig := hash.Hash(inp.Prog)
	sign := inp.Signal.Deserialize()
	fuzzer.addInputToCorpus(p, sign, sig)
}

// Add to corresponding queue according to its flag
func (fuzzer *Fuzzer) addS2EInputToCorpus(p *prog.Prog, flag uint32, sig hash.Sig) {
	fuzzer.corpusMu.Lock()
	if _, ok := fuzzer.corpusHashes[sig]; !ok {
		if flag|CAP_OFFSET != 0 {
			fuzzer.corpus[SEED_OFFSET] = append(fuzzer.corpus[SEED_OFFSET], p)
		}
		if flag|CAP_VALUES != 0 {
			fuzzer.corpus[SEED_VALUE] = append(fuzzer.corpus[SEED_VALUE], p)
		}
		if flag|CAP_LENGTH != 0 {
			fuzzer.corpus[SEED_LENGTH] = append(fuzzer.corpus[SEED_LENGTH], p)
		}
		if flag|CAP_SPOT != 0 {
			// FIXME
		}
		fuzzer.corpusHashes[sig] = struct{}{}
	}
	fuzzer.corpusMu.Unlock()
}

func (fuzzer *Fuzzer) addInputToCorpus(p *prog.Prog, sign signal.Signal, sig hash.Sig) {
	fuzzer.corpusMu.Lock()
	if _, ok := fuzzer.corpusHashes[sig]; !ok {
		fuzzer.corpus[SEED_COVERAGE] = append(fuzzer.corpus[SEED_COVERAGE], p)
		fuzzer.corpusHashes[sig] = struct{}{}
	}
	fuzzer.corpusMu.Unlock()

	if !sign.Empty() {
		fuzzer.signalMu.Lock()
		fuzzer.corpusSignal.Merge(sign)
		fuzzer.maxSignal.Merge(sign)
		fuzzer.signalMu.Unlock()
	}
}

// uniformally select a non-empty queue
func (fuzzer *Fuzzer) corpusSnapshot() []*prog.Prog {
	fuzzer.corpusMu.RLock()
	defer fuzzer.corpusMu.RUnlock()
	var indices []int
	for i := 0; i < len(fuzzer.corpus); i++ {
		if len(fuzzer.corpus[i]) != 0 {
			indices = append(indices, i)
		}
	}
	return fuzzer.corpus[indices[fuzzer.rnd.Intn(len(indices))]]
}

func (fuzzer *Fuzzer) addMaxSignal(sign signal.Signal) {
	if sign.Len() == 0 {
		return
	}
	fuzzer.signalMu.Lock()
	defer fuzzer.signalMu.Unlock()
	fuzzer.maxSignal.Merge(sign)
}

func (fuzzer *Fuzzer) grabNewSignal() signal.Signal {
	fuzzer.signalMu.Lock()
	defer fuzzer.signalMu.Unlock()
	sign := fuzzer.newSignal
	if sign.Empty() {
		return nil
	}
	fuzzer.newSignal = nil
	return sign
}

func (fuzzer *Fuzzer) corpusSignalDiff(sign signal.Signal) signal.Signal {
	fuzzer.signalMu.RLock()
	defer fuzzer.signalMu.RUnlock()
	return fuzzer.corpusSignal.Diff(sign)
}

func (fuzzer *Fuzzer) checkNewSignal(p *prog.Prog, info *ipc.ProgInfo) (calls []int, extra bool) {
	fuzzer.signalMu.RLock()
	defer fuzzer.signalMu.RUnlock()
	for i, inf := range info.Calls {
		if fuzzer.checkNewCallSignal(p, &inf, i) {
			calls = append(calls, i)
		}
	}
	extra = fuzzer.checkNewCallSignal(p, &info.Extra, -1)
	return
}

func (fuzzer *Fuzzer) checkNewCallSignal(p *prog.Prog, info *ipc.CallInfo, call int) bool {
	diff := fuzzer.maxSignal.DiffRaw(info.Signal, signalPrio(p, info, call))
	if diff.Empty() {
		return false
	}
	fuzzer.signalMu.RUnlock()
	fuzzer.signalMu.Lock()
	fuzzer.maxSignal.Merge(diff)
	fuzzer.newSignal.Merge(diff)
	fuzzer.signalMu.Unlock()
	fuzzer.signalMu.RLock()
	return true
}

func signalPrio(p *prog.Prog, info *ipc.CallInfo, call int) (prio uint8) {
	if call == -1 {
		return 0
	}
	if info.Errno == 0 {
		prio |= 1 << 1
	}
	if !p.Target.CallContainsAny(p.Calls[call]) {
		prio |= 1 << 0
	}
	return
}

func parseOutputType(str string) OutputType {
	switch str {
	case "none":
		return OutputNone
	case "stdout":
		return OutputStdout
	case "dmesg":
		return OutputDmesg
	case "file":
		return OutputFile
	default:
		log.Fatalf("-output flag must be one of none/stdout/dmesg/file")
		return OutputNone
	}
}