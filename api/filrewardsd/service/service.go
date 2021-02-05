package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	logging "github.com/ipfs/go-log/v2"
	nutil "github.com/textileio/go-threads/net/util"
	"github.com/textileio/go-threads/util"
	analytics "github.com/textileio/textile/v2/api/analyticsd/client"
	analyticspb "github.com/textileio/textile/v2/api/analyticsd/pb"
	pb "github.com/textileio/textile/v2/api/filrewardsd/pb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	rewardsCollectionName = "filrewards"
	claimsCollectionName  = "filclaims"
)

var log = logging.Logger("filrewards")

var rewardTypeMeta = map[pb.RewardType]meta{
	pb.RewardType_REWARD_TYPE_FIRST_KEY_ACCOUNT_CREATED:    {factor: 3},
	pb.RewardType_REWARD_TYPE_FIRST_KEY_USER_CREATED:       {factor: 1},
	pb.RewardType_REWARD_TYPE_FIRST_ORG_CREATED:            {factor: 3},
	pb.RewardType_REWARD_TYPE_INITIAL_BILLING_SETUP:        {factor: 1},
	pb.RewardType_REWARD_TYPE_FIRST_BUCKET_CREATED:         {factor: 2},
	pb.RewardType_REWARD_TYPE_FIRST_BUCKET_ARCHIVE_CREATED: {factor: 2},
	pb.RewardType_REWARD_TYPE_FIRST_MAILBOX_CREATED:        {factor: 1},
	pb.RewardType_REWARD_TYPE_FIRST_THREAD_DB_CREATED:      {factor: 1},
}

type meta struct {
	factor int32
}

type reward struct {
	OrgKey            string        `bson:"org_key"`
	DevKey            string        `bson:"dev_key"`
	Type              pb.RewardType `bson:"type"`
	Factor            int32         `bson:"factor"`
	BaseAttoFILReward int32         `bson:"base_atto_fil_reward"`
	CreatedAt         time.Time     `bson:"created_at"`
}

type claim struct {
	OrgKey    string    `bson:"org_key"`
	ClaimedBy string    `bson:"claimed_by"`
	Amount    int32     `bson:"amount"`
	CreatedAt time.Time `bson:"created_at"`
}

type orgKeyLock string

func (l orgKeyLock) Key() string {
	return string(l)
}

var _ nutil.SemaphoreKey = (*orgKeyLock)(nil)

var _ pb.FilRewardsServiceServer = (*Service)(nil)

type Service struct {
	rewardsCol        *mongo.Collection
	claimsCol         *mongo.Collection
	ac                *analytics.Client
	rewardsCacheOrg   map[string]map[pb.RewardType]struct{}
	rewardsCacheDev   map[string]map[pb.RewardType]struct{}
	baseAttoFILReward int32
	server            *grpc.Server
	semaphores        *nutil.SemaphorePool
}

type Config struct {
	Listener          net.Listener
	MongoUri          string
	MongoDbName       string
	AnalyticsAddr     string
	BaseAttoFILReward int32
	Debug             bool
}

func New(ctx context.Context, config Config) (*Service, error) {
	if config.Debug {
		if err := util.SetLogLevels(map[string]logging.LogLevel{
			"filrewards": logging.LevelDebug,
		}); err != nil {
			return nil, err
		}
	}

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(config.MongoUri))
	if err != nil {
		return nil, fmt.Errorf("connecting to mongo: %v", err)
	}
	db := client.Database(config.MongoDbName)
	rewardsCol := db.Collection(rewardsCollectionName)
	claimsCol := db.Collection(claimsCollectionName)
	_, err = rewardsCol.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{primitive.E{Key: "org_key", Value: 1}, primitive.E{Key: "type", Value: 1}},
			Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"org_key": bson.M{"$gt": ""}}),
		},
		{
			Keys:    bson.D{primitive.E{Key: "dev_key", Value: 1}, primitive.E{Key: "type", Value: 1}},
			Options: options.Index().SetUnique(true).SetPartialFilterExpression(bson.M{"dev_key": bson.M{"$gt": ""}}),
		},
		{
			Keys: bson.D{primitive.E{Key: "org_key", Value: 1}},
		},
		{
			Keys: bson.D{primitive.E{Key: "dev_key", Value: 1}},
		},
		{
			Keys: bson.D{primitive.E{Key: "type", Value: 1}},
		},
		{
			Keys: bson.D{primitive.E{Key: "created_at", Value: 1}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating indexes: %v", err)
	}

	// Populate caches.
	opts := options.Find()
	cursor, err := rewardsCol.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, fmt.Errorf("querying RewardRecords to populate cache: %s", err)
	}
	defer cursor.Close(ctx)
	cacheOrg := make(map[string]map[pb.RewardType]struct{})
	cacheDev := make(map[string]map[pb.RewardType]struct{})
	for cursor.Next(ctx) {
		var rec reward
		if err := cursor.Decode(&rec); err != nil {
			return nil, fmt.Errorf("decoding RewardRecord while building cache: %v", err)
		}
		ensureKeyRewardCache(cacheOrg, rec.OrgKey)
		ensureKeyRewardCache(cacheDev, rec.DevKey)
		cacheOrg[rec.OrgKey][rec.Type] = struct{}{}
		cacheOrg[rec.DevKey][rec.Type] = struct{}{}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterating cursor while building cache: %v", err)
	}

	s := &Service{
		rewardsCol:        rewardsCol,
		claimsCol:         claimsCol,
		rewardsCacheOrg:   cacheOrg,
		rewardsCacheDev:   cacheDev,
		baseAttoFILReward: config.BaseAttoFILReward,
		semaphores:        nutil.NewSemaphorePool(1),
	}

	if config.AnalyticsAddr != "" {
		s.ac, err = analytics.New(config.AnalyticsAddr)
		if err != nil {
			return nil, fmt.Errorf("creating analytics client: %s", err)
		}
	}

	s.server = grpc.NewServer()
	go func() {
		pb.RegisterFilRewardsServiceServer(s.server, s)
		if err := s.server.Serve(config.Listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Errorf("serve error: %v", err)
		}
	}()

	return s, nil
}

func (s *Service) ProcessAnalyticsEvent(ctx context.Context, req *pb.ProcessAnalyticsEventRequest) (*pb.ProcessAnalyticsEventResponse, error) {
	if req.OrgKey == "" {
		return nil, status.Error(codes.InvalidArgument, "must provide org key")
	}
	if req.AnalyticsEvent == analyticspb.Event_EVENT_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "must provide analytics event")
	}

	lck := s.semaphores.Get(orgKeyLock(req.OrgKey))
	lck.Acquire()
	defer lck.Release()

	t := pb.RewardType_REWARD_TYPE_UNSPECIFIED
	switch req.AnalyticsEvent {
	case analyticspb.Event_EVENT_KEY_ACCOUNT_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_KEY_ACCOUNT_CREATED
	case analyticspb.Event_EVENT_KEY_USER_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_KEY_USER_CREATED
	case analyticspb.Event_EVENT_ORG_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_ORG_CREATED
	case analyticspb.Event_EVENT_BILLING_SETUP:
		t = pb.RewardType_REWARD_TYPE_INITIAL_BILLING_SETUP
	case analyticspb.Event_EVENT_BUCKET_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_BUCKET_CREATED
	case analyticspb.Event_EVENT_BUCKET_ARCHIVE_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_BUCKET_ARCHIVE_CREATED
	case analyticspb.Event_EVENT_MAILBOX_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_MAILBOX_CREATED
	case analyticspb.Event_EVENT_THREAD_DB_CREATED:
		t = pb.RewardType_REWARD_TYPE_FIRST_THREAD_DB_CREATED
	}
	if t == pb.RewardType_REWARD_TYPE_UNSPECIFIED {
		// It is normal to get an analytics event we aren't interested in, so just return an empty result and no error.
		return &pb.ProcessAnalyticsEventResponse{}, nil
	}

	ensureKeyRewardCache(s.rewardsCacheOrg, req.OrgKey)
	ensureKeyRewardCache(s.rewardsCacheDev, req.DevKey)

	if _, exists := s.rewardsCacheOrg[req.OrgKey][t]; exists {
		// This reward is already granted to the Org so bail.
		return &pb.ProcessAnalyticsEventResponse{}, nil
	}

	if _, exists := s.rewardsCacheDev[req.DevKey][t]; exists {
		// This reward is already granted to the Dev so bail.
		return &pb.ProcessAnalyticsEventResponse{}, nil
	}

	r := &reward{
		OrgKey:            req.OrgKey,
		DevKey:            req.DevKey,
		Type:              t,
		Factor:            rewardTypeMeta[t].factor,
		BaseAttoFILReward: s.baseAttoFILReward,
		CreatedAt:         time.Now(),
	}

	if _, err := s.rewardsCol.InsertOne(ctx, r); err != nil {
		return nil, status.Errorf(codes.Internal, "inserting reward: %v", err)
	}

	s.rewardsCacheOrg[req.OrgKey][t] = struct{}{}
	s.rewardsCacheDev[req.DevKey][t] = struct{}{}

	if err := s.ac.Track(
		ctx,
		r.OrgKey,
		analyticspb.AccountType_ACCOUNT_TYPE_ORG,
		analyticspb.Event_EVENT_FIL_REWARD,
		analytics.WithProperties(map[string]interface{}{
			"type":                 r.Type,
			"factor":               rewardTypeMeta[r.Type].factor,
			"base_atto_fil_reward": s.baseAttoFILReward,
			"amount":               rewardTypeMeta[r.Type].factor * s.baseAttoFILReward,
			"dev_key":              req.DevKey,
		}),
	); err != nil {
		log.Errorf("calling analytics track: %v", err)
	}

	return &pb.ProcessAnalyticsEventResponse{Reward: toPbReward(r)}, nil
}

func (s *Service) ListRewards(ctx context.Context, req *pb.ListRewardsRequest) (*pb.ListRewardsResponse, error) {
	findOpts := options.Find()
	if req.Limit > 0 {
		findOpts.Limit = &req.Limit
	}
	sort := -1
	if req.Ascending {
		sort = 1
	}
	findOpts.Sort = bson.D{primitive.E{Key: "created_at", Value: sort}}
	filter := bson.M{}
	if req.OrgKeyFilter != "" {
		filter["org_key"] = req.OrgKeyFilter
	}
	if req.DevKeyFilter != "" {
		filter["dev_key"] = req.DevKeyFilter
	}
	if req.RewardTypeFilter != pb.RewardType_REWARD_TYPE_UNSPECIFIED {
		filter["type"] = req.RewardTypeFilter
	}
	comp := "$lt"
	if req.StartAt != nil {
		if req.Ascending {
			comp = "$gt"
		}
		t := req.StartAt.AsTime()
		filter["created_at"] = bson.M{comp: &t}
	}
	cursor, err := s.rewardsCol.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "querying rewards: %v", err)
	}
	defer cursor.Close(ctx)
	var rewards []reward
	err = cursor.All(ctx, &rewards)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decoding reward query results: %v", err)
	}

	more := false
	var startAt *time.Time
	if len(rewards) > 0 {
		lastCreatedAt := &rewards[len(rewards)-1].CreatedAt
		filter["created_at"] = bson.M{comp: *lastCreatedAt}
		res := s.rewardsCol.FindOne(ctx, filter)
		if res.Err() != nil && res.Err() != mongo.ErrNoDocuments {
			return nil, status.Errorf(codes.Internal, "checking for more data: %v", err)
		}
		if res.Err() != mongo.ErrNoDocuments {
			more = true
			startAt = lastCreatedAt
		}
	}
	var pbRewards []*pb.Reward
	for _, rec := range rewards {
		pbRewards = append(pbRewards, toPbReward(&rec))
	}
	res := &pb.ListRewardsResponse{
		Rewards: pbRewards,
		More:    more,
	}
	if startAt != nil {
		res.MoreStartAt = timestamppb.New(*startAt)
	}
	return res, nil
}

func (s *Service) Claim(ctx context.Context, req *pb.ClaimRequest) (*pb.ClaimResponse, error) {
	lck := s.semaphores.Get(orgKeyLock(req.OrgKey))
	lck.Acquire()
	defer lck.Release()

	totalRewarded, err := s.totalRewarded(ctx, req.OrgKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "calculating total rewarded: %v", err)
	}
	totalClaimed, err := s.totalClaimed(ctx, req.OrgKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "calculating total claimed: %v", err)
	}

	available := totalRewarded - totalClaimed

	if req.Amount > available {
		return nil, status.Errorf(codes.InvalidArgument, "claim amount %d is greater than available reward balance %d", req.Amount, available)
	}

	c := &claim{
		OrgKey:    req.OrgKey,
		ClaimedBy: req.ClaimedBy,
		Amount:    req.Amount,
		CreatedAt: time.Now(),
	}

	if _, err := s.claimsCol.InsertOne(ctx, c); err != nil {
		return nil, status.Errorf(codes.Internal, "inserting claim: %v", err)
	}

	if err := s.ac.Track(
		ctx,
		c.OrgKey,
		analyticspb.AccountType_ACCOUNT_TYPE_ORG,
		analyticspb.Event_EVENT_FIL_CLAIM,
		analytics.WithProperties(map[string]interface{}{
			"claimed_by": c.ClaimedBy,
			"amount":     c.Amount,
		}),
	); err != nil {
		log.Errorf("calling analytics track: %v", err)
	}

	return &pb.ClaimResponse{}, nil
}

func (s *Service) ListClaims(ctx context.Context, req *pb.ListClaimsRequest) (*pb.ListClaimsResponse, error) {
	findOpts := options.Find()
	if req.Limit > 0 {
		findOpts.Limit = &req.Limit
	}
	sort := -1
	if req.Ascending {
		sort = 1
	}
	findOpts.Sort = bson.D{primitive.E{Key: "created_at", Value: sort}}
	filter := bson.M{}
	if req.OrgKeyFilter != "" {
		filter["org_key"] = req.OrgKeyFilter
	}
	if req.ClaimedByFilter != "" {
		filter["claimed_by"] = req.ClaimedByFilter
	}
	comp := "$lt"
	if req.StartAt != nil {
		if req.Ascending {
			comp = "$gt"
		}
		t := req.StartAt.AsTime()
		filter["created_at"] = bson.M{comp: &t}
	}
	cursor, err := s.claimsCol.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "querying claims: %v", err)
	}
	defer cursor.Close(ctx)
	var claims []claim
	err = cursor.All(ctx, &claims)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "decoding claim query results: %v", err)
	}

	more := false
	var startAt *time.Time
	if len(claims) > 0 {
		lastCreatedAt := &claims[len(claims)-1].CreatedAt
		filter["created_at"] = bson.M{comp: *lastCreatedAt}
		res := s.claimsCol.FindOne(ctx, filter)
		if res.Err() != nil && res.Err() != mongo.ErrNoDocuments {
			return nil, status.Errorf(codes.Internal, "checking for more data: %v", err)
		}
		if res.Err() != mongo.ErrNoDocuments {
			more = true
			startAt = lastCreatedAt
		}
	}
	var pbClaims []*pb.Claim
	for _, rec := range claims {
		pbClaims = append(pbClaims, toPbClaim(&rec))
	}
	res := &pb.ListClaimsResponse{
		Claims: pbClaims,
		More:   more,
	}
	if startAt != nil {
		res.MoreStartAt = timestamppb.New(*startAt)
	}
	return res, nil
}

func (s *Service) Balance(ctx context.Context, req *pb.BalanceRequest) (*pb.BalanceResponse, error) {
	lck := s.semaphores.Get(orgKeyLock(req.OrgKey))
	lck.Acquire()
	defer lck.Release()

	totalRewarded, err := s.totalRewarded(ctx, req.OrgKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "calculating total rewarded: %v", err)
	}
	totalClaimed, err := s.totalClaimed(ctx, req.OrgKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "calculating total claimed: %v", err)
	}
	return &pb.BalanceResponse{Rewarded: totalRewarded, Claimed: totalClaimed, Available: totalRewarded - totalClaimed}, nil
}

func (s *Service) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.rewardsCol.Database().Client().Disconnect(ctx); err != nil {
		log.Errorf("disconnecting mongo client: %s", err)
	}
	log.Info("mongo client disconnected")

	stopped := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(stopped)
	}()
	t := time.NewTimer(10 * time.Second)
	select {
	case <-t.C:
		s.server.Stop()
	case <-stopped:
		t.Stop()
	}
	log.Info("gRPC server stopped")
}

func (s *Service) totalRewarded(ctx context.Context, orgKey string) (int32, error) {
	cursor, err := s.rewardsCol.Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"org_key": orgKey}},
		bson.M{"$project": bson.M{"amt": bson.M{"$multiply": bson.A{"$factor", "$base_atto_fil_reward"}}}},
		bson.M{"$group": bson.M{"_id": nil, "total": bson.M{"$sum": "$amt"}}},
	})
	if err != nil {
		return -1, err
	}
	for cursor.Next(ctx) {
		elements, err := cursor.Current.Elements()
		if err != nil {
			return -1, err
		}
		for _, e := range elements {
			if e.Key() == "total" {
				return e.Value().Int32(), nil
			}
		}
	}
	return -1, fmt.Errorf("no total rewarded calculation found")
}

func (s *Service) totalClaimed(ctx context.Context, orgKey string) (int32, error) {
	cursor, err := s.rewardsCol.Aggregate(ctx, bson.A{
		bson.M{"$match": bson.M{"org_key": orgKey}},
		bson.M{"$group": bson.M{"_id": nil, "total": bson.M{"$sum": "$amount"}}},
	})
	if err != nil {
		return -1, err
	}
	for cursor.Next(ctx) {
		elements, err := cursor.Current.Elements()
		if err != nil {
			return -1, err
		}
		for _, e := range elements {
			if e.Key() == "total" {
				return e.Value().Int32(), nil
			}
		}
	}
	return -1, fmt.Errorf("no total claimed calculation found")
}

func (s *Service) get(ctx context.Context, orgKey string, t pb.RewardType) (*reward, error) {
	filter := bson.M{"org_key": orgKey, "type": t}
	res := s.rewardsCol.FindOne(ctx, filter)
	if res.Err() != nil {
		return nil, res.Err()
	}
	var r reward
	if err := res.Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func toPbReward(rec *reward) *pb.Reward {
	res := &pb.Reward{
		OrgKey:            rec.OrgKey,
		DevKey:            rec.DevKey,
		Type:              rec.Type,
		Factor:            rec.Factor,
		BaseAttoFilReward: rec.BaseAttoFILReward,
		CreatedAt:         timestamppb.New(rec.CreatedAt),
	}
	return res
}

func toPbClaim(rec *claim) *pb.Claim {
	res := &pb.Claim{
		OrgKey:    rec.OrgKey,
		ClaimedBy: rec.ClaimedBy,
		Amount:    rec.Amount,
		CreatedAt: timestamppb.New(rec.CreatedAt),
	}
	return res
}

func ensureKeyRewardCache(keyCache map[string]map[pb.RewardType]struct{}, key string) {
	if _, exists := keyCache[key]; !exists {
		keyCache[key] = map[pb.RewardType]struct{}{}
	}
}