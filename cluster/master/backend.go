package master

import (
	"errors"
	"fmt"
	"github.com/QuarkChain/goquarkchain/account"
	"github.com/QuarkChain/goquarkchain/cluster/config"
	"github.com/QuarkChain/goquarkchain/cluster/rpc"
	"github.com/QuarkChain/goquarkchain/cluster/service"
	"github.com/QuarkChain/goquarkchain/consensus"
	"github.com/QuarkChain/goquarkchain/consensus/doublesha256"
	"github.com/QuarkChain/goquarkchain/consensus/qkchash"
	"github.com/QuarkChain/goquarkchain/core"
	"github.com/QuarkChain/goquarkchain/core/types"
	"github.com/QuarkChain/goquarkchain/internal/qkcapi"
	"github.com/QuarkChain/goquarkchain/p2p"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	ethRPC "github.com/ethereum/go-ethereum/rpc"
	"golang.org/x/sync/errgroup"
	"math/big"
	"os"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"time"
)

const heartbeatInterval = time.Duration(4 * time.Second)

var (
	ErrNoBranchConn = errors.New("no such branch's connection")
)

// QKCMasterBackend masterServer include connections
type QKCMasterBackend struct {
	lock               sync.RWMutex
	engine             consensus.Engine
	eventMux           *event.TypeMux
	chainDb            ethdb.Database
	shutdown           chan os.Signal
	clusterConfig      *config.ClusterConfig
	clientPool         map[string]*SlaveConnection
	branchToSlaves     map[uint32][]*SlaveConnection
	branchToShardStats map[uint32]*rpc.ShardStats
	artificialTxConfig *rpc.ArtificialTxConfig
	rootBlockChain     *core.RootBlockChain
	logInfo            string
}

// New new master with config
func New(ctx *service.ServiceContext, cfg *config.ClusterConfig) (*QKCMasterBackend, error) {
	var (
		mstr = &QKCMasterBackend{
			clusterConfig:      cfg,
			eventMux:           ctx.EventMux,
			clientPool:         make(map[string]*SlaveConnection),
			branchToSlaves:     make(map[uint32][]*SlaveConnection, 0),
			branchToShardStats: make(map[uint32]*rpc.ShardStats),
			artificialTxConfig: &rpc.ArtificialTxConfig{
				TargetRootBlockTime:  cfg.Quarkchain.Root.ConsensusConfig.TargetBlockTime,
				TargetMinorBlockTime: cfg.Quarkchain.GetShardConfigByFullShardID(cfg.Quarkchain.GetGenesisShardIds()[0]).ConsensusConfig.TargetBlockTime,
			},
			logInfo:  "masterServer",
			shutdown: ctx.Shutdown,
		}
		err error
	)
	if mstr.chainDb, err = createDB(ctx, cfg.DbPathRoot); err != nil {
		return nil, err
	}

	if mstr.engine, err = createConsensusEngine(ctx, cfg.Quarkchain.Root); err != nil {
		return nil, err
	}

	genesis := core.NewGenesis(cfg.Quarkchain)
	genesis.MustCommitRootBlock(mstr.chainDb)
	if mstr.rootBlockChain, err = core.NewRootBlockChain(mstr.chainDb, nil, cfg.Quarkchain, mstr.engine, nil); err != nil {
		return nil, err
	}

	for _, cfg := range cfg.SlaveList {
		target := fmt.Sprintf("%s:%d", cfg.IP, cfg.Port)
		client := NewSlaveConn(target, cfg.ChainMaskList, cfg.ID)
		mstr.clientPool[target] = client
	}
	log.Info("qkc api backend", "slave client pool", len(mstr.clientPool))

	return mstr, nil
}

func createDB(ctx *service.ServiceContext, name string) (ethdb.Database, error) {
	db, err := ctx.OpenDatabase(name, 128, 1024) // TODO @liuhuan to delete "128 1024"?
	if err != nil {
		return nil, err
	}
	return db, nil
}

func createConsensusEngine(ctx *service.ServiceContext, cfg *config.RootConfig) (consensus.Engine, error) {
	diffCalculator := consensus.EthDifficultyCalculator{
		MinimumDifficulty: big.NewInt(int64(cfg.Genesis.Difficulty)),
		AdjustmentCutoff:  cfg.DifficultyAdjustmentCutoffTime,
		AdjustmentFactor:  cfg.DifficultyAdjustmentFactor,
	}
	switch cfg.ConsensusType {
	case config.PoWFake:
		return &consensus.FakeEngine{}, nil
	case config.PoWEthash, config.PoWSimulate:
		panic(errors.New("not support PoWEthash PoWSimulate"))
	case config.PoWQkchash:
		return qkchash.New(cfg.ConsensusConfig.RemoteMine, &diffCalculator, cfg.ConsensusConfig.RemoteMine), nil
	case config.PoWDoubleSha256:
		return doublesha256.New(&diffCalculator, cfg.ConsensusConfig.RemoteMine), nil
	}
	return nil, fmt.Errorf("Failed to create consensus engine consensus type %s", cfg.ConsensusType)
}

func (s *QKCMasterBackend) GetClusterConfig() *config.ClusterConfig {
	return s.clusterConfig
}

// Protocols p2p protocols, p2p Server will start in node.Start
func (s *QKCMasterBackend) Protocols() []p2p.Protocol {
	// TODO add p2p.protocol
	return nil
}

// APIs return all apis for master Server
func (s *QKCMasterBackend) APIs() []ethRPC.API {
	apis := qkcapi.GetAPIs(s)
	return append(apis, []ethRPC.API{
		{
			Namespace: "rpc." + reflect.TypeOf(QKCMasterServerSideOp{}).Name(),
			Version:   "3.0",
			Service:   NewServerSideOp(s),
			Public:    false,
		},
	}...)
}

// Stop stop node -> stop qkcMaster
func (s *QKCMasterBackend) Stop() error {
	if s.engine != nil {
		s.engine.Close()
	}
	s.eventMux.Stop()
	return nil
}

// Start start node -> start qkcMaster
func (s *QKCMasterBackend) Start(srvr *p2p.Server) error {
	if err := s.InitCluster(); err != nil {
		return err
	}
	// start heart beat pre 3 seconds.
	s.Heartbeat()
	return nil
}

// StartMining start mining
func (s *QKCMasterBackend) StartMining(threads int) error {
	// TODO @liuhuan
	return nil
}

// StopMining stop mining
func (s *QKCMasterBackend) StopMining(threads int) error {
	// TODO @liuhuan
	return nil
}

// InitCluster init cluster :
// 1:ConnectToSlaves
// 2:logSummary
// 3:check if has all shards
// 4.setup slave to slave
// 5:init shards
func (s *QKCMasterBackend) InitCluster() error {
	if err := s.ConnectToSlaves(); err != nil {
		return err
	}
	s.logSummary()
	if err := s.hasAllShards(); err != nil {
		return err
	}
	if err := s.initShards(); err != nil {
		return err
	}
	return nil
}

func (s *QKCMasterBackend) ConnectToSlaves() error {
	fullShardIds := s.clusterConfig.Quarkchain.GetGenesisShardIds()
	for _, slaveConn := range s.clientPool {
		id, chainMaskList, err := slaveConn.SendPing(nil, false)
		if err != nil {
			return err
		}
		if err := checkPing(slaveConn, id, chainMaskList); err != nil {
			return err
		}
		for _, fullShardID := range fullShardIds {
			if slaveConn.hasShard(fullShardID) {
				s.branchToSlaves[fullShardID] = append(s.branchToSlaves[fullShardID], slaveConn)
			}
		}
	}
	return nil
}
func (s *QKCMasterBackend) logSummary() {
	for branch, slaves := range s.branchToSlaves {
		for _, slave := range slaves {
			log.Info(s.logInfo, "branch:", branch, "is run by slave", slave.slaveID)
		}
	}
}

func (s *QKCMasterBackend) hasAllShards() error {
	if len(s.branchToSlaves) == len(s.clusterConfig.Quarkchain.GetGenesisShardIds()) {
		for _, v := range s.branchToSlaves {
			if len(v) == 0 {
				return errors.New("branch's slave<=0")
			}
		}
		return nil
	}
	return errors.New("len not match")
}

func (s *QKCMasterBackend) getSlaveInfoListFromClusterConfig() []*rpc.SlaveInfo {
	slaveInfos := make([]*rpc.SlaveInfo, 0)
	for _, slave := range s.clusterConfig.SlaveList {
		slaveInfos = append(slaveInfos, &rpc.SlaveInfo{
			Id:            []byte(slave.ID),
			Host:          []byte(slave.IP),
			Port:          slave.Port,
			ChainMaskList: slave.ChainMaskList,
		})
	}
	return slaveInfos
}
func (s *QKCMasterBackend) initShards() error {
	var g errgroup.Group
	for index := range s.clientPool {
		i := index
		g.Go(func() error {
			currRootBlock := s.rootBlockChain.CurrentBlock()
			_, _, err := s.clientPool[i].SendPing(currRootBlock, true)
			return err
		})
	}
	return g.Wait()
}

func (s *QKCMasterBackend) Heartbeat() {
	go func(normal bool) {
		for normal {
			timeGap := time.Now()
			for endpoint := range s.clientPool {
				normal = s.clientPool[endpoint].HeartBeat()
				if !normal {
					s.shutdown <- syscall.SIGTERM
					break
				}
			}
			duration := time.Now().Sub(timeGap)
			log.Trace(s.logInfo, "heart beat duration", duration.String())
			time.Sleep(heartbeatInterval)
		}
	}(true)
	//TODO :add send master info
}

func checkPing(slaveConn *SlaveConnection, id []byte, chainMaskList []*types.ChainMask) error {
	if slaveConn.slaveID != string(id) {
		return errors.New("slaveID is not match")
	}
	if len(chainMaskList) != len(slaveConn.shardMaskList) {
		return errors.New("chainMaskList is not match")
	}
	lenChainMaskList := len(chainMaskList)

	for index := 0; index < lenChainMaskList; index++ {
		if chainMaskList[index].GetMask() != slaveConn.shardMaskList[index].GetMask() {
			return errors.New("chainMaskList index is not match")
		}
	}
	return nil
}

func (s *QKCMasterBackend) getOneSlaveConnection(branch account.Branch) *SlaveConnection {
	slaves := s.branchToSlaves[branch.Value]
	if len(slaves) < 1 {
		return nil
	}
	return slaves[0]
}

func (s *QKCMasterBackend) getAllSlaveConnection(fullShardID uint32) []*SlaveConnection {
	slaves := s.branchToSlaves[fullShardID]
	if len(slaves) < 1 {
		return nil
	}
	return slaves
}

func (s *QKCMasterBackend) createRootBlockToMine(address account.Address) (*types.RootBlock, error) {
	var g errgroup.Group
	rspList := make(map[string]*rpc.GetUnconfirmedHeadersResponse)

	for target := range s.clientPool {
		target := target
		g.Go(func() error {
			rsp, err := s.clientPool[target].GetUnconfirmedHeaders()
			rspList[target] = rsp
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	fullShardIDToHeaderList := make(map[uint32][]*types.MinorBlockHeader, 0)
	for _, resp := range rspList {
		for _, headersInfo := range resp.HeadersInfoList {
			if _, ok := fullShardIDToHeaderList[headersInfo.Branch.Value]; ok { // to avoid overlap
				continue // skip it if has added
			}
			height := uint64(0)
			for _, header := range headersInfo.HeaderList {
				if height != 0 && height+1 != header.Number {
					return nil, errors.New("headers must ordered by height")
				}
				height = header.Number

				if !s.rootBlockChain.IsMinorBlockValidated(header.Hash()) {
					break
				}
				fullShardIDToHeaderList[headersInfo.Branch.Value] = append(fullShardIDToHeaderList[headersInfo.Branch.Value], header)
			}
		}
	}

	headerList := make([]*types.MinorBlockHeader, 0)
	currTipHeight := s.rootBlockChain.CurrentBlock().Number()
	fullShardIdToCheck := s.clusterConfig.Quarkchain.GetInitializedShardIdsBeforeRootHeight(currTipHeight + 1)
	sort.Slice(fullShardIdToCheck, func(i, j int) bool { return fullShardIdToCheck[i] < fullShardIdToCheck[j] })
	for _, fullShardID := range fullShardIdToCheck {
		headers := fullShardIDToHeaderList[fullShardID]
		headerList = append(headerList, headers...)
	}
	newblock, err := s.rootBlockChain.CreateBlockToMine(headerList, &address, nil)
	if err != nil {
		return nil, err
	}
	if err := s.rootBlockChain.Validator().ValidateBlock(newblock); err != nil {
		//TODO :only for exposure problem ,need to delete later
		panic(err)
	}
	return newblock, nil
}

// GetAccountData get account Data for jsonRpc
func (s *QKCMasterBackend) GetAccountData(address account.Address, height *uint64) (map[account.Branch]*rpc.AccountBranchData, error) {
	var g errgroup.Group
	rspList := make(map[string]*rpc.GetAccountDataResponse)
	for target := range s.clientPool {
		target := target
		g.Go(func() error {
			rsp, err := s.clientPool[target].GetAccountData(address, height)
			rspList[target] = rsp
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	branchToAccountBranchData := make(map[account.Branch]*rpc.AccountBranchData)
	for _, rsp := range rspList {
		for _, accountBranchData := range rsp.AccountBranchDataList {
			branchToAccountBranchData[accountBranchData.Branch] = accountBranchData
		}
	}
	if len(branchToAccountBranchData) != len(s.clusterConfig.Quarkchain.GetGenesisShardIds()) {
		return nil, errors.New("len is not match")
	}
	return branchToAccountBranchData, nil
}

// GetPrimaryAccountData get primary account data for jsonRpc
func (s *QKCMasterBackend) GetPrimaryAccountData(address account.Address, blockHeight *uint64) (*rpc.AccountBranchData, error) {
	fullShardID := s.clusterConfig.Quarkchain.GetFullShardIdByFullShardKey(address.FullShardKey)
	slaveConn := s.getOneSlaveConnection(account.Branch{Value: fullShardID})
	if slaveConn == nil {
		return nil, ErrNoBranchConn
	}
	rsp, err := slaveConn.GetAccountData(address, blockHeight)
	if err != nil {
		return nil, err
	}
	for _, accountBranchData := range rsp.AccountBranchDataList {
		if accountBranchData.Branch.Value == fullShardID {
			return accountBranchData, nil
		}
	}
	return nil, errors.New("no such data")
}

// SendMiningConfigToSlaves send mining config to slaves,used in jsonRpc
func (s *QKCMasterBackend) SendMiningConfigToSlaves(mining bool) error {
	var g errgroup.Group
	for index := range s.clientPool {
		i := index
		g.Go(func() error {
			return s.clientPool[i].SendMiningConfigToSlaves(s.artificialTxConfig, mining)
		})
	}
	return g.Wait()
}

// AddRootBlock add root block to all slaves
func (s *QKCMasterBackend) AddRootBlock(rootBlock *types.RootBlock) error {
	s.rootBlockChain.WriteCommittingHash(rootBlock.Hash())
	_, err := s.rootBlockChain.InsertChain([]types.IBlock{rootBlock})
	if err != nil {
		return err
	}
	var g errgroup.Group
	for index := range s.clientPool {
		i := index
		g.Go(func() error {
			return s.clientPool[i].AddRootBlock(rootBlock, false)
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	s.rootBlockChain.ClearCommittingHash()
	return nil
}

// SetTargetBlockTime set target Time from jsonRpc
func (s *QKCMasterBackend) SetTargetBlockTime(rootBlockTime *uint32, minorBlockTime *uint32) error {
	if rootBlockTime == nil {
		temp := s.artificialTxConfig.TargetMinorBlockTime
		rootBlockTime = &temp
	}

	if minorBlockTime == nil {
		temp := s.artificialTxConfig.TargetMinorBlockTime
		minorBlockTime = &temp
	}
	s.artificialTxConfig = &rpc.ArtificialTxConfig{
		TargetRootBlockTime:  *rootBlockTime,
		TargetMinorBlockTime: *minorBlockTime,
	}
	return s.StartMining(1)
}

// SetMining setmiming status
func (s *QKCMasterBackend) SetMining(mining bool) error {
	//TODO need liuhuan to finish
	if mining {
		return s.StartMining(1)
	}
	return s.StopMining(1)
}

// CreateTransactions Create transactions and add to the network for load testing
func (s *QKCMasterBackend) CreateTransactions(numTxPerShard, xShardPercent uint32, tx *types.Transaction) error {
	var g errgroup.Group
	for index := range s.clientPool {
		i := index
		g.Go(func() error {
			return s.clientPool[i].GenTx(numTxPerShard, xShardPercent, tx)
		})
	}
	return g.Wait()
}

// UpdateShardStatus update shard status for branchg
func (s *QKCMasterBackend) UpdateShardStatus(status *rpc.ShardStats) {
	s.lock.Lock()
	s.branchToShardStats[status.Branch.Value] = status
	s.lock.Unlock()
}

// UpdateTxCountHistory update Tx count queue
func (s *QKCMasterBackend) UpdateTxCountHistory(txCount, xShardTxCount uint32, createTime uint64) {
	// TODO @scf next pr to implement
	panic("not implement")
}

func (s *QKCMasterBackend) GetBlockCount() map[string]interface{} {
	// TODO @scf next pr to implement
	panic("not implement")
}

func (s *QKCMasterBackend) GetStats() map[string]interface{} {
	// TODO @scf next pr to implement
	panic("not implement")
}

func (s *QKCMasterBackend) isSyning() bool {
	// TODO @liuhuan
	return false
}

func (s *QKCMasterBackend) isMining() bool {
	// TODO @liuhuan
	return false
}

func (s *QKCMasterBackend) CurrentBlock() *types.RootBlock {
	return s.rootBlockChain.CurrentBlock()
}
