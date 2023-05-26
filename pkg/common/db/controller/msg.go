package controller

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/config"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/constant"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/convert"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/db/cache"
	unRelationTb "github.com/OpenIMSDK/Open-IM-Server/pkg/common/db/table/unrelation"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/db/unrelation"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/kafka"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/log"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/common/prome"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/errs"

	"context"
	"errors"

	pbMsg "github.com/OpenIMSDK/Open-IM-Server/pkg/proto/msg"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/proto/sdkws"
	"github.com/OpenIMSDK/Open-IM-Server/pkg/utils"
	"github.com/go-redis/redis/v8"
	"go.mongodb.org/mongo-driver/mongo"
)

type CommonMsgDatabase interface {
	// 批量插入消息
	BatchInsertChat2DB(ctx context.Context, conversationID string, msgs []*sdkws.MsgData, currentMaxSeq int64) error
	// 撤回消息
	RevokeMsg(ctx context.Context, conversationID string, seq int64, msg []byte) error
	// 刪除redis中消息缓存
	DeleteMessageFromCache(ctx context.Context, conversationID string, msgs []*sdkws.MsgData) error
	// incrSeq然后批量插入缓存
	BatchInsertChat2Cache(ctx context.Context, conversationID string, msgs []*sdkws.MsgData) (seq int64, isNewConversation bool, err error)

	//  通过seqList获取mongo中写扩散消息
	GetMsgBySeqsRange(ctx context.Context, userID string, conversationID string, begin, end, num int64) (seqMsg []*sdkws.MsgData, err error)
	// 通过seqList获取大群在 mongo里面的消息
	GetMsgBySeqs(ctx context.Context, userID string, conversationID string, seqs []int64) (seqMsg []*sdkws.MsgData, err error)

	// 删除会话消息重置最小seq， remainTime为消息保留的时间单位秒,超时消息删除， 传0删除所有消息(此方法不删除redis cache)
	DeleteConversationMsgsAndSetMinSeq(ctx context.Context, conversationID string, remainTime int64) error

	SetMaxSeq(ctx context.Context, conversationID string, maxSeq int64) error
	GetMaxSeqs(ctx context.Context, conversationIDs []string) (map[string]int64, error)
	GetMaxSeq(ctx context.Context, conversationID string) (int64, error)
	SetMinSeq(ctx context.Context, conversationID string, minSeq int64) error
	GetMinSeqs(ctx context.Context, conversationIDs []string) (map[string]int64, error)
	GetMinSeq(ctx context.Context, conversationID string) (int64, error)
	GetConversationUserMinSeq(ctx context.Context, conversationID string, userID string) (int64, error)
	GetConversationUserMinSeqs(ctx context.Context, conversationID string, userIDs []string) (map[string]int64, error)
	SetConversationUserMinSeq(ctx context.Context, conversationID string, userID string, minSeq int64) error
	SetConversationUserMinSeqs(ctx context.Context, conversationID string, seqs map[string]int64) (err error)
	SetUserConversationsMinSeqs(ctx context.Context, userID string, seqs map[string]int64) (err error)

	GetMongoMaxAndMinSeq(ctx context.Context, conversationID string) (maxSeq, minSeq int64, err error)
	GetConversationMinMaxSeqInMongoAndCache(ctx context.Context, conversationID string) (minSeqMongo, maxSeqMongo, minSeqCache, maxSeqCache int64, err error)
	SetSendMsgStatus(ctx context.Context, id string, status int32) error
	GetSendMsgStatus(ctx context.Context, id string) (int32, error)

	// to mq
	MsgToMQ(ctx context.Context, key string, msg2mq *sdkws.MsgData) error
	MsgToModifyMQ(ctx context.Context, key, conversarionID string, msgs []*sdkws.MsgData) error
	MsgToPushMQ(ctx context.Context, key, conversarionID string, msg2mq *sdkws.MsgData) (int32, int64, error)
	MsgToMongoMQ(ctx context.Context, key, conversarionID string, msgs []*sdkws.MsgData, lastSeq int64) error

	// modify
	JudgeMessageReactionExist(ctx context.Context, clientMsgID string, sessionType int32) (bool, error)
	SetMessageTypeKeyValue(ctx context.Context, clientMsgID string, sessionType int32, typeKey, value string) error
	SetMessageReactionExpire(ctx context.Context, clientMsgID string, sessionType int32, expiration time.Duration) (bool, error)
	GetExtendMsg(ctx context.Context, conversationID string, sessionType int32, clientMsgID string, maxMsgUpdateTime int64) (*pbMsg.ExtendMsg, error)
	InsertOrUpdateReactionExtendMsgSet(ctx context.Context, conversationID string, sessionType int32, clientMsgID string, msgFirstModifyTime int64, reactionExtensionList map[string]*sdkws.KeyValue) error
	GetMessageTypeKeyValue(ctx context.Context, clientMsgID string, sessionType int32, typeKey string) (string, error)
	GetOneMessageAllReactionList(ctx context.Context, clientMsgID string, sessionType int32) (map[string]string, error)
	DeleteOneMessageKey(ctx context.Context, clientMsgID string, sessionType int32, subKey string) error
	DeleteReactionExtendMsgSet(ctx context.Context, conversationID string, sessionType int32, clientMsgID string, msgFirstModifyTime int64, reactionExtensionList map[string]*sdkws.KeyValue) error
}

func NewCommonMsgDatabase(msgDocModel unRelationTb.MsgDocModelInterface, cacheModel cache.MsgModel) CommonMsgDatabase {
	return &commonMsgDatabase{
		msgDocDatabase:   msgDocModel,
		cache:            cacheModel,
		producer:         kafka.NewKafkaProducer(config.Config.Kafka.Ws2mschat.Addr, config.Config.Kafka.Ws2mschat.Topic),
		producerToMongo:  kafka.NewKafkaProducer(config.Config.Kafka.MsgToMongo.Addr, config.Config.Kafka.MsgToMongo.Topic),
		producerToPush:   kafka.NewKafkaProducer(config.Config.Kafka.Ms2pschat.Addr, config.Config.Kafka.Ms2pschat.Topic),
		producerToModify: kafka.NewKafkaProducer(config.Config.Kafka.MsgToModify.Addr, config.Config.Kafka.MsgToModify.Topic),
	}
}

func InitCommonMsgDatabase(rdb redis.UniversalClient, database *mongo.Database) CommonMsgDatabase {
	cacheModel := cache.NewMsgCacheModel(rdb)
	msgDocModel := unrelation.NewMsgMongoDriver(database)
	CommonMsgDatabase := NewCommonMsgDatabase(msgDocModel, cacheModel)
	return CommonMsgDatabase
}

type commonMsgDatabase struct {
	msgDocDatabase    unRelationTb.MsgDocModelInterface
	extendMsgDatabase unRelationTb.ExtendMsgSetModelInterface
	extendMsgSetModel unRelationTb.ExtendMsgSetModel
	msg               unRelationTb.MsgDocModel
	cache             cache.MsgModel
	producer          *kafka.Producer
	producerToMongo   *kafka.Producer
	producerToModify  *kafka.Producer
	producerToPush    *kafka.Producer
}

func (db *commonMsgDatabase) MsgToMQ(ctx context.Context, key string, msg2mq *sdkws.MsgData) error {
	_, _, err := db.producer.SendMessage(ctx, key, msg2mq)
	return err
}

func (db *commonMsgDatabase) MsgToModifyMQ(ctx context.Context, key, conversationID string, messages []*sdkws.MsgData) error {
	if len(messages) > 0 {
		_, _, err := db.producerToModify.SendMessage(ctx, key, &pbMsg.MsgDataToModifyByMQ{ConversationID: conversationID, Messages: messages})
		return err
	}
	return nil
}

func (db *commonMsgDatabase) MsgToPushMQ(ctx context.Context, key, conversationID string, msg2mq *sdkws.MsgData) (int32, int64, error) {
	partition, offset, err := db.producerToPush.SendMessage(ctx, key, &pbMsg.PushMsgDataToMQ{MsgData: msg2mq, ConversationID: conversationID})
	if err != nil {
		log.ZError(ctx, "MsgToPushMQ", err, "key", key, "msg2mq", msg2mq)
		return 0, 0, err
	}
	return partition, offset, nil
}

func (db *commonMsgDatabase) MsgToMongoMQ(ctx context.Context, key, conversationID string, messages []*sdkws.MsgData, lastSeq int64) error {
	if len(messages) > 0 {
		_, _, err := db.producerToMongo.SendMessage(ctx, key, &pbMsg.MsgDataToMongoByMQ{LastSeq: lastSeq, ConversationID: conversationID, MsgData: messages})
		return err
	}
	return nil
}

func (db *commonMsgDatabase) BatchInsertBlock(ctx context.Context, conversationID string, msgList []*unRelationTb.MsgInfoModel, firstSeq int64) error {
	if len(msgList) == 0 {
		return nil
	}
	num := db.msg.GetSingleGocMsgNum()
	num = 100
	if msgList[0].Msg != nil {
		firstSeq = msgList[0].Msg.Seq
	}
	getDocID := func(seq int64) string {
		return conversationID + ":" + strconv.FormatInt(seq/num, 10)
	}
	getIndex := func(seq int64) int64 {
		return seq % num
	}
	// 返回值为true表示数据库存在该文档，false表示数据库不存在该文档
	updateMsgModel := func(docID string, index int64, msg *unRelationTb.MsgInfoModel) (bool, error) {
		var (
			res *mongo.UpdateResult
			err error
		)
		if msg.Msg != nil {
			res, err = db.msgDocDatabase.UpdateMsg(ctx, docID, index, "msg", msg.Msg)
		} else if msg.Revoke != nil {
			res, err = db.msgDocDatabase.UpdateMsg(ctx, docID, index, "revoke", msg.Revoke)
		} else if msg.DelList != nil {
			res, err = db.msgDocDatabase.PushUnique(ctx, docID, index, "del_list", msg.DelList)
			// } else if msg.ReadList != nil {
			// 	res, err = db.msgDocDatabase.PushUnique(ctx, docID, index, "read_list", msg.ReadList)
		} else {
			return false, errs.ErrArgs.Wrap("msg all field is nil")
		}
		if err != nil {
			return false, err
		}
		return res.MatchedCount > 0, nil
	}
	tryUpdate := true
	for i := 0; i < len(msgList); i++ {
		msg := msgList[i]
		seq := firstSeq + int64(i)
		docID := getDocID(seq)
		if tryUpdate {
			matched, err := updateMsgModel(docID, getIndex(seq), msg)
			if err != nil {
				return err
			}
			if matched {
				continue
			}
		}
		doc := unRelationTb.MsgDocModel{
			DocID: docID,
			Msg:   make([]*unRelationTb.MsgInfoModel, num),
		}
		var insert int
		for j := i; j < len(msgList); j++ {
			seq = firstSeq + int64(j)
			if getDocID(seq) != docID {
				break
			}
			insert++
			doc.Msg[getIndex(seq)] = msgList[j]
		}
		for i, model := range doc.Msg {
			if model.DelList == nil {
				doc.Msg[i].DelList = []string{}
			}
			// if model.ReadList == nil {
			// 	doc.Msg[i].ReadList = []string{}
			// }
		}
		if err := db.msgDocDatabase.Create(ctx, &doc); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				i--
				tryUpdate = true
				continue
			}
			return err
		}
		tryUpdate = false
		i += insert - 1
	}
	return nil
}

func (db *commonMsgDatabase) BatchInsertChat2DB(ctx context.Context, conversationID string, msgList []*sdkws.MsgData, currentMaxSeq int64) error {
	//num := db.msg.GetSingleGocMsgNum()
	//currentIndex := currentMaxSeq / num
	//var blockMsgs []*[]*sdkws.MsgData
	//for i, data := range msgList {
	//	data.Seq = currentMaxSeq + int64(i+1)
	//	index := data.Seq/num - currentIndex
	//	if i == 0 && index == 1 {
	//		index--
	//		currentIndex++
	//	}
	//	var block *[]*sdkws.MsgData
	//	if len(blockMsgs) == int(index) {
	//		var size int64
	//		if i == 0 {
	//			size = num - data.Seq%num
	//		} else {
	//			temp := int64(len(msgList)-len(*blockMsgs[0])) - int64(len(blockMsgs)-1)*num
	//			if temp >= num {
	//				size = num
	//			} else {
	//				size = temp % num
	//			}
	//		}
	//		temp := make([]*sdkws.MsgData, 0, size)
	//		block = &temp
	//		blockMsgs = append(blockMsgs, block)
	//	} else {
	//		block = blockMsgs[index]
	//	}
	//	*block = append(*block, msgList[i])
	//}
	//create := currentMaxSeq == 0 || ((*blockMsgs[0])[0].Seq%num == 0)
	//if !create {
	//	exist, err := db.msgDocDatabase.IsExistDocID(ctx, db.msg.IndexDocID(conversationID, currentIndex))
	//	if err != nil {
	//		return err
	//	}
	//	create = !exist
	//}
	//for i, msgs := range blockMsgs {
	//	docID := db.msg.IndexDocID(conversationID, currentIndex+int64(i))
	//	if create || i != 0 { // 插入
	//		doc := unRelationTb.MsgDocModel{
	//			DocID: docID,
	//			Msg:   make([]unRelationTb.MsgInfoModel, num),
	//		}
	//		for i := 0; i < len(doc.Msg); i++ {
	//			doc.Msg[i].ReadList = []string{}
	//			doc.Msg[i].DelList = []string{}
	//		}
	//		for _, msg := range *msgs {
	//			data, err := proto.Marshal(msg)
	//			if err != nil {
	//				return err
	//			}
	//			doc.Msg[msg.Seq%num] = unRelationTb.MsgInfoModel{
	//				SendTime: msg.SendTime,
	//				Msg:      data,
	//				ReadList: []string{},
	//				DelList:  []string{},
	//			}
	//		}
	//		if err := db.msgDocDatabase.Create(ctx, &doc); err != nil {
	//			prome.Inc(prome.MsgInsertMongoFailedCounter)
	//			return utils.Wrap(err, "")
	//		}
	//		prome.Inc(prome.MsgInsertMongoSuccessCounter)
	//	} else { // 修改
	//		for _, msg := range *msgs {
	//			data, err := proto.Marshal(msg)
	//			if err != nil {
	//				return err
	//			}
	//			info := unRelationTb.MsgInfoModel{
	//				SendTime: msg.SendTime,
	//				Msg:      data,
	//			}
	//			if err := db.msgDocDatabase.UpdateMsg(ctx, docID, msg.Seq%num, &info); err != nil {
	//				prome.Inc(prome.MsgInsertMongoFailedCounter)
	//				return err
	//			}
	//			prome.Inc(prome.MsgInsertMongoSuccessCounter)
	//		}
	//	}
	//}
	return nil
}

func (db *commonMsgDatabase) RevokeMsg(ctx context.Context, conversationID string, seq int64, msg []byte) error {
	index := seq / db.msg.GetSingleGocMsgNum()
	docID := db.msg.IndexDocID(conversationID, index)
	return db.msgDocDatabase.UpdateMsgContent(ctx, docID, seq%db.msg.GetSingleGocMsgNum(), msg)
}

func (db *commonMsgDatabase) DeleteMessageFromCache(ctx context.Context, conversationID string, msgs []*sdkws.MsgData) error {
	return db.cache.DeleteMessageFromCache(ctx, conversationID, msgs)
}

func (db *commonMsgDatabase) BatchInsertChat2Cache(ctx context.Context, conversationID string, msgs []*sdkws.MsgData) (seq int64, isNew bool, err error) {
	currentMaxSeq, err := db.cache.GetMaxSeq(ctx, conversationID)
	if err != nil && errs.Unwrap(err) != redis.Nil {
		prome.Inc(prome.SeqGetFailedCounter)
		return 0, false, err
	}
	prome.Inc(prome.SeqGetSuccessCounter)
	lenList := len(msgs)
	if int64(lenList) > db.msg.GetSingleGocMsgNum() {
		return 0, false, errors.New("too large")
	}
	if lenList < 1 {
		return 0, false, errors.New("too short as 0")
	}
	if errs.Unwrap(err) == redis.Nil {
		isNew = true
	}
	lastMaxSeq := currentMaxSeq
	for _, m := range msgs {
		currentMaxSeq++
		m.Seq = currentMaxSeq
	}
	failedNum, err := db.cache.SetMessageToCache(ctx, conversationID, msgs)
	if err != nil {
		prome.Add(prome.MsgInsertRedisFailedCounter, failedNum)
		log.ZError(ctx, "setMessageToCache error", err, "len", len(msgs), "conversationID", conversationID)
	} else {
		prome.Inc(prome.MsgInsertRedisSuccessCounter)
	}
	err = db.cache.SetMaxSeq(ctx, conversationID, currentMaxSeq)
	if err != nil {
		prome.Inc(prome.SeqSetFailedCounter)
	} else {
		prome.Inc(prome.SeqSetSuccessCounter)
	}
	return lastMaxSeq, isNew, utils.Wrap(err, "")
}

func (db *commonMsgDatabase) getMsgBySeqs(ctx context.Context, conversationID string, seqs []int64) (totalMsgs []*sdkws.MsgData, err error) {
	var totalUnExistSeqs []int64
	for docID, seqs := range db.msg.GetDocIDSeqsMap(conversationID, seqs) {
		log.ZDebug(ctx, "getMsgBySeqs", "docID", docID, "seqs", seqs)
		msgs, unexistSeqs, err := db.findMsgInfoBySeq(ctx, docID, seqs)
		if err != nil {
			return nil, err
		}
		for _, msg := range msgs {
			totalMsgs = append(totalMsgs, convert.MsgDB2Pb(msg.Msg))
		}
		totalUnExistSeqs = append(totalUnExistSeqs, unexistSeqs...)
	}
	for _, unexistSeq := range totalUnExistSeqs {
		totalMsgs = append(totalMsgs, db.msg.GenExceptionMessageBySeqs([]int64{unexistSeq})...)
	}
	return totalMsgs, nil
}

func (db *commonMsgDatabase) refetchDelSeqsMsgs(ctx context.Context, conversationID string, delNums, rangeBegin, begin int64) (seqMsgs []*unRelationTb.MsgDataModel, err error) {
	var reFetchSeqs []int64
	if delNums > 0 {
		newBeginSeq := rangeBegin - delNums
		if newBeginSeq >= begin {
			newEndSeq := rangeBegin - 1
			for i := newBeginSeq; i <= newEndSeq; i++ {
				reFetchSeqs = append(reFetchSeqs, i)
			}
		}
	}
	if len(reFetchSeqs) == 0 {
		return
	}
	if len(reFetchSeqs) > 0 {
		// m := db.msg.GetDocIDSeqsMap(conversationID, reFetchSeqs)
		// for docID, seqs := range m {
		// 	msgs, _, err := db.findMsgInfoBySeq(ctx, docID, seqs)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// 	for _, msg := range msgs {
		// 		if msg.Status != constant.MsgDeleted {
		// 			seqMsgs = append(seqMsgs, msg)
		// 		}
		// 	}
		// }
	}
	if len(seqMsgs) < int(delNums) {
		seqMsgs2, err := db.refetchDelSeqsMsgs(ctx, conversationID, delNums-int64(len(seqMsgs)), rangeBegin-1, begin)
		if err != nil {
			return seqMsgs, err
		}
		seqMsgs = append(seqMsgs, seqMsgs2...)
	}
	return seqMsgs, nil
}

func (db *commonMsgDatabase) findMsgInfoBySeq(ctx context.Context, docID string, seqs []int64) (totalMsgs []*unRelationTb.MsgInfoModel, unExistSeqs []int64, err error) {
	msgs, err := db.msgDocDatabase.GetMsgBySeqIndexIn1Doc(ctx, docID, seqs)
	if err != nil {
		return nil, nil, err
	}
	log.ZDebug(ctx, "findMsgInfoBySeq", "docID", docID, "seqs", seqs, "len(msgs)", len(msgs))
	totalMsgs = append(totalMsgs, msgs...)
	if len(msgs) == 0 {
		unExistSeqs = seqs
	} else {
		for _, seq := range seqs {
			for i, msg := range msgs {
				if seq == msg.Msg.Seq {
					break
				}
				if i == len(msgs)-1 {
					unExistSeqs = append(unExistSeqs, seq)
				}
			}
		}
	}
	return totalMsgs, unExistSeqs, nil
}

func (db *commonMsgDatabase) getMsgBySeqsRange(ctx context.Context, userID string, conversationID string, allSeqs []int64, begin, end int64) (seqMsgs []*sdkws.MsgData, err error) {
	log.ZDebug(ctx, "getMsgBySeqsRange", "conversationID", conversationID, "allSeqs", allSeqs, "begin", begin, "end", end)
	var totalNotExistSeqs []int64
	// mongo index
	var delSeqs []int64
	for docID, seqs := range db.msg.GetDocIDSeqsMap(conversationID, allSeqs) {
		log.ZDebug(ctx, "getMsgBySeqsRange", "docID", docID, "seqs", seqs)
		msgs, notExistSeqs, err := db.findMsgInfoBySeq(ctx, docID, seqs)
		if err != nil {
			return nil, err
		}
		log.ZDebug(ctx, "getMsgBySeqsRange", "unExistSeqs", notExistSeqs, "msgs", len(msgs))
		for _, msg := range msgs {
			if utils.IsContain(userID, msg.DelList) {
				delSeqs = append(delSeqs, msg.Msg.Seq)
			}
			seqMsgs = append(seqMsgs, convert.MsgDB2Pb(msg.Msg))
		}
		totalNotExistSeqs = append(totalNotExistSeqs, notExistSeqs...)
	}
	log.ZDebug(ctx, "getMsgBySeqsRange", "totalNotExistSeqs", totalNotExistSeqs)
	seqMsgs = append(seqMsgs, db.msg.GenExceptionMessageBySeqs(totalNotExistSeqs)...)
	for _, msg := range seqMsgs {
		if msg.Status == constant.MsgDeleted {
			delSeqs = append(delSeqs, msg.Seq)
		}
	}
	if len(delSeqs) > 0 {
		// msgs, err := db.refetchDelSeqsMsgs(ctx, conversationID, int64(len(delSeqs)), allSeqs[0], begin)
		// if err != nil {
		// 	log.ZWarn(ctx, "refetchDelSeqsMsgs", err, "delSeqs", delSeqs, "begin", begin)
		// }
		// for _, msg := range msgs {
		// 	seqMsgs = append(seqMsgs, convert.MsgDB2Pb(msg))
		// }
	}
	// sort by seq
	if len(totalNotExistSeqs) > 0 || len(delSeqs) > 0 {
		sort.Sort(utils.MsgBySeq(seqMsgs))
	}
	return seqMsgs, nil
}

func (db *commonMsgDatabase) GetMsgBySeqsRange(ctx context.Context, userID string, conversationID string, begin, end, num int64) (seqMsg []*sdkws.MsgData, err error) {
	var seqs []int64
	for i := end; i > end-num; i-- {
		if i >= begin {
			seqs = append([]int64{i}, seqs...)
		} else {
			break
		}
	}
	successMsgs, failedSeqs, err := db.cache.GetMessagesBySeq(ctx, conversationID, seqs)
	if err != nil {
		if err != redis.Nil {
			prome.Add(prome.MsgPullFromRedisFailedCounter, len(failedSeqs))
			log.ZError(ctx, "get message from redis exception", err, conversationID, seqs)
		}
	}
	if len(failedSeqs) != 0 {
		log.ZDebug(ctx, "get message from redis failed", err, "seqs", seqs)
	}
	// get from cache or db
	prome.Add(prome.MsgPullFromRedisSuccessCounter, len(successMsgs))
	if len(failedSeqs) > 0 {
		mongoMsgs, err := db.getMsgBySeqsRange(ctx, userID, conversationID, failedSeqs, begin, end)
		if err != nil {
			prome.Add(prome.MsgPullFromMongoFailedCounter, len(failedSeqs))
			return nil, err
		}
		prome.Add(prome.MsgPullFromMongoSuccessCounter, len(mongoMsgs))
		successMsgs = append(successMsgs, mongoMsgs...)
	}
	return successMsgs, nil
}

func (db *commonMsgDatabase) GetMsgBySeqs(ctx context.Context, userID string, conversationID string, seqs []int64) (successMsgs []*sdkws.MsgData, err error) {
	userMinSeq, err := db.cache.GetConversationUserMinSeq(ctx, conversationID, userID)
	if err != nil {
		return nil, err
	}
	minSeq, err := db.cache.GetMinSeq(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	if userMinSeq < minSeq {
		minSeq = userMinSeq
	}
	var newSeqs []int64
	for _, seq := range seqs {
		if seq >= minSeq {
			newSeqs = append(newSeqs, seq)
		}
	}
	successMsgs, failedSeqs, err := db.cache.GetMessagesBySeq(ctx, conversationID, newSeqs)
	if err != nil {
		if err != redis.Nil {
			prome.Add(prome.MsgPullFromRedisFailedCounter, len(failedSeqs))
			log.ZError(ctx, "get message from redis exception", err, "failedSeqs", failedSeqs, "conversationID", conversationID)
		}
	}
	prome.Add(prome.MsgPullFromRedisSuccessCounter, len(successMsgs))
	if len(failedSeqs) > 0 {
		mongoMsgs, err := db.getMsgBySeqs(ctx, conversationID, failedSeqs)
		if err != nil {
			prome.Add(prome.MsgPullFromMongoFailedCounter, len(failedSeqs))
			return nil, err
		}
		prome.Add(prome.MsgPullFromMongoSuccessCounter, len(mongoMsgs))
		successMsgs = append(successMsgs, mongoMsgs...)
	}
	return successMsgs, nil
}

func (db *commonMsgDatabase) DeleteConversationMsgsAndSetMinSeq(ctx context.Context, conversationID string, remainTime int64) error {
	var delStruct delMsgRecursionStruct
	var skip int64
	minSeq, err := db.deleteMsgRecursion(ctx, conversationID, skip, &delStruct, remainTime)
	if err != nil {
		return err
	}
	if minSeq == 0 {
		return nil
	}
	if remainTime == 0 {
		err = db.cache.CleanUpOneConversationAllMsg(ctx, conversationID)
		if err != nil {
			log.ZWarn(ctx, "CleanUpOneUserAllMsg", err, "conversationID", conversationID)
		}
	}
	return db.cache.SetMinSeq(ctx, conversationID, minSeq)
}

// this is struct for recursion
type delMsgRecursionStruct struct {
	minSeq    int64
	delDocIDs []string
}

func (d *delMsgRecursionStruct) getSetMinSeq() int64 {
	return d.minSeq
}

// index 0....19(del) 20...69
// seq 70
// set minSeq 21
// recursion 删除list并且返回设置的最小seq
func (db *commonMsgDatabase) deleteMsgRecursion(ctx context.Context, conversationID string, index int64, delStruct *delMsgRecursionStruct, remainTime int64) (int64, error) {
	// find from oldest list
	msgDocModel, err := db.msgDocDatabase.GetMsgDocModelByIndex(ctx, conversationID, index, 1)
	if err != nil || msgDocModel.DocID == "" {
		if err != nil {
			if err == unrelation.ErrMsgListNotExist {
				log.ZDebug(ctx, "deleteMsgRecursion ErrMsgListNotExist", "conversationID", conversationID, "index:", index)
			} else {
				log.ZError(ctx, "deleteMsgRecursion GetUserMsgListByIndex failed", err, "conversationID", conversationID, "index", index)
			}
		}
		// 获取报错，或者获取不到了，物理删除并且返回seq delMongoMsgsPhysical(delStruct.delDocIDList), 结束递归
		err = db.msgDocDatabase.DeleteDocs(ctx, delStruct.delDocIDs)
		if err != nil {
			return 0, err
		}
		return delStruct.getSetMinSeq() + 1, nil
	}
	log.ZDebug(ctx, "doc info", "conversationID", conversationID, "index", index, "docID", msgDocModel.DocID, "len", len(msgDocModel.Msg))
	if int64(len(msgDocModel.Msg)) > db.msg.GetSingleGocMsgNum() {
		log.ZWarn(ctx, "msgs too large", nil, "lenth", len(msgDocModel.Msg), "docID:", msgDocModel.DocID)
	}
	if msgDocModel.Msg[len(msgDocModel.Msg)-1].Msg.SendTime+(remainTime*1000) < utils.GetCurrentTimestampByMill() && msgDocModel.IsFull() {
		delStruct.delDocIDs = append(delStruct.delDocIDs, msgDocModel.DocID)
		delStruct.minSeq = msgDocModel.Msg[len(msgDocModel.Msg)-1].Msg.Seq
	} else {
		var hasMarkDelFlag bool
		var delMsgIndexs []int
		for i, MsgInfoModel := range msgDocModel.Msg {
			if MsgInfoModel != nil {
				if utils.GetCurrentTimestampByMill() > MsgInfoModel.Msg.SendTime+(remainTime*1000) {
					delMsgIndexs = append(delMsgIndexs, i)
					hasMarkDelFlag = true
				} else {
					// 到本条消息不需要删除, minSeq置为这条消息的seq
					if err := db.msgDocDatabase.DeleteDocs(ctx, delStruct.delDocIDs); err != nil {
						return 0, err
					}
					if hasMarkDelFlag {
						// mark del all delMsgIndexs
						if err := db.msgDocDatabase.DeleteMsgsInOneDocByIndex(ctx, msgDocModel.DocID, delMsgIndexs); err != nil {
							return delStruct.getSetMinSeq(), err
						}
					}
					return MsgInfoModel.Msg.Seq, nil
				}
			}
		}
	}
	//  继续递归 index+1
	seq, err := db.deleteMsgRecursion(ctx, conversationID, index+1, delStruct, remainTime)
	return seq, err
}

func (db *commonMsgDatabase) CleanUpUserConversationsMsgs(ctx context.Context, user string, conversationIDs []string) {
	for _, conversationID := range conversationIDs {
		maxSeq, err := db.cache.GetMaxSeq(ctx, conversationID)
		if err != nil {
			if err == redis.Nil {
				log.ZInfo(ctx, "max seq is nil", "conversationID", conversationID)
			} else {
				log.ZError(ctx, "get max seq failed", err, "conversationID", conversationID)
			}
			continue
		}
		if err := db.cache.SetMinSeq(ctx, conversationID, maxSeq+1); err != nil {
			log.ZError(ctx, "set min seq failed", err, "conversationID", conversationID, "minSeq", maxSeq+1)
		}
	}
}

func (db *commonMsgDatabase) SetMaxSeq(ctx context.Context, conversationID string, maxSeq int64) error {
	return db.cache.SetMaxSeq(ctx, conversationID, maxSeq)
}
func (db *commonMsgDatabase) GetMaxSeqs(ctx context.Context, conversationIDs []string) (map[string]int64, error) {
	return db.cache.GetMaxSeqs(ctx, conversationIDs)
}
func (db *commonMsgDatabase) GetMaxSeq(ctx context.Context, conversationID string) (int64, error) {
	return db.cache.GetMaxSeq(ctx, conversationID)
}
func (db *commonMsgDatabase) SetMinSeq(ctx context.Context, conversationID string, minSeq int64) error {
	return db.cache.SetMinSeq(ctx, conversationID, minSeq)
}
func (db *commonMsgDatabase) GetMinSeqs(ctx context.Context, conversationIDs []string) (map[string]int64, error) {
	return db.cache.GetMinSeqs(ctx, conversationIDs)
}
func (db *commonMsgDatabase) GetMinSeq(ctx context.Context, conversationID string) (int64, error) {
	return db.cache.GetMinSeq(ctx, conversationID)
}
func (db *commonMsgDatabase) GetConversationUserMinSeq(ctx context.Context, conversationID string, userID string) (int64, error) {
	return db.cache.GetConversationUserMinSeq(ctx, conversationID, userID)
}
func (db *commonMsgDatabase) GetConversationUserMinSeqs(ctx context.Context, conversationID string, userIDs []string) (map[string]int64, error) {
	return db.cache.GetConversationUserMinSeqs(ctx, conversationID, userIDs)
}
func (db *commonMsgDatabase) SetConversationUserMinSeq(ctx context.Context, conversationID string, userID string, minSeq int64) error {
	return db.cache.SetConversationUserMinSeq(ctx, conversationID, userID, minSeq)
}
func (db *commonMsgDatabase) SetConversationUserMinSeqs(ctx context.Context, conversationID string, seqs map[string]int64) (err error) {
	return db.cache.SetConversationUserMinSeqs(ctx, conversationID, seqs)
}

func (db *commonMsgDatabase) SetUserConversationsMinSeqs(ctx context.Context, userID string, seqs map[string]int64) error {
	return db.cache.SetUserConversationsMinSeqs(ctx, userID, seqs)
}

func (db *commonMsgDatabase) SetSendMsgStatus(ctx context.Context, id string, status int32) error {
	return db.cache.SetSendMsgStatus(ctx, id, status)
}

func (db *commonMsgDatabase) GetSendMsgStatus(ctx context.Context, id string) (int32, error) {
	return db.cache.GetSendMsgStatus(ctx, id)
}

func (db *commonMsgDatabase) GetConversationMinMaxSeqInMongoAndCache(ctx context.Context, conversationID string) (minSeqMongo, maxSeqMongo, minSeqCache, maxSeqCache int64, err error) {
	minSeqMongo, maxSeqMongo, err = db.GetMinMaxSeqMongo(ctx, conversationID)
	if err != nil {
		return
	}
	// from cache
	minSeqCache, err = db.cache.GetMinSeq(ctx, conversationID)
	if err != nil {
		return
	}
	maxSeqCache, err = db.cache.GetMaxSeq(ctx, conversationID)
	if err != nil {
		return
	}
	return
}

func (db *commonMsgDatabase) GetMongoMaxAndMinSeq(ctx context.Context, conversationID string) (maxSeq, minSeq int64, err error) {
	return db.GetMinMaxSeqMongo(ctx, conversationID)
}

func (db *commonMsgDatabase) GetMinMaxSeqMongo(ctx context.Context, conversationID string) (minSeqMongo, maxSeqMongo int64, err error) {
	oldestMsgMongo, err := db.msgDocDatabase.GetOldestMsg(ctx, conversationID)
	if err != nil {
		return
	}
	minSeqMongo = oldestMsgMongo.Msg.Seq
	newestMsgMongo, err := db.msgDocDatabase.GetNewestMsg(ctx, conversationID)
	if err != nil {
		return
	}
	maxSeqMongo = newestMsgMongo.Msg.Seq
	return
}

func (db *commonMsgDatabase) JudgeMessageReactionExist(ctx context.Context, clientMsgID string, sessionType int32) (bool, error) {
	return db.cache.JudgeMessageReactionExist(ctx, clientMsgID, sessionType)
}

func (db *commonMsgDatabase) SetMessageTypeKeyValue(ctx context.Context, clientMsgID string, sessionType int32, typeKey, value string) error {
	return db.cache.SetMessageTypeKeyValue(ctx, clientMsgID, sessionType, typeKey, value)
}

func (db *commonMsgDatabase) SetMessageReactionExpire(ctx context.Context, clientMsgID string, sessionType int32, expiration time.Duration) (bool, error) {
	return db.cache.SetMessageReactionExpire(ctx, clientMsgID, sessionType, expiration)
}

func (db *commonMsgDatabase) GetMessageTypeKeyValue(ctx context.Context, clientMsgID string, sessionType int32, typeKey string) (string, error) {
	return db.cache.GetMessageTypeKeyValue(ctx, clientMsgID, sessionType, typeKey)
}

func (db *commonMsgDatabase) GetOneMessageAllReactionList(ctx context.Context, clientMsgID string, sessionType int32) (map[string]string, error) {
	return db.cache.GetOneMessageAllReactionList(ctx, clientMsgID, sessionType)
}

func (db *commonMsgDatabase) DeleteOneMessageKey(ctx context.Context, clientMsgID string, sessionType int32, subKey string) error {
	return db.cache.DeleteOneMessageKey(ctx, clientMsgID, sessionType, subKey)
}

func (db *commonMsgDatabase) InsertOrUpdateReactionExtendMsgSet(ctx context.Context, conversationID string, sessionType int32, clientMsgID string, msgFirstModifyTime int64, reactionExtensions map[string]*sdkws.KeyValue) error {
	return db.extendMsgDatabase.InsertOrUpdateReactionExtendMsgSet(ctx, conversationID, sessionType, clientMsgID, msgFirstModifyTime, db.extendMsgSetModel.Pb2Model(reactionExtensions))
}

func (db *commonMsgDatabase) GetExtendMsg(ctx context.Context, conversationID string, sessionType int32, clientMsgID string, maxMsgUpdateTime int64) (*pbMsg.ExtendMsg, error) {
	extendMsgSet, err := db.extendMsgDatabase.GetExtendMsgSet(ctx, conversationID, sessionType, maxMsgUpdateTime)
	if err != nil {
		return nil, err
	}
	extendMsg, ok := extendMsgSet.ExtendMsgs[clientMsgID]
	if !ok {
		return nil, errs.ErrRecordNotFound.Wrap(fmt.Sprintf("cant find client msg id: %s", clientMsgID))
	}
	reactionExtensionList := make(map[string]*pbMsg.KeyValueResp)
	for key, model := range extendMsg.ReactionExtensionList {
		reactionExtensionList[key] = &pbMsg.KeyValueResp{
			KeyValue: &sdkws.KeyValue{
				TypeKey:          model.TypeKey,
				Value:            model.Value,
				LatestUpdateTime: model.LatestUpdateTime,
			},
		}
	}
	return &pbMsg.ExtendMsg{
		ReactionExtensions: reactionExtensionList,
		ClientMsgID:        extendMsg.ClientMsgID,
		MsgFirstModifyTime: extendMsg.MsgFirstModifyTime,
		AttachedInfo:       extendMsg.AttachedInfo,
		Ex:                 extendMsg.Ex,
	}, nil
}

func (db *commonMsgDatabase) DeleteReactionExtendMsgSet(ctx context.Context, conversationID string, sessionType int32, clientMsgID string, msgFirstModifyTime int64, reactionExtensions map[string]*sdkws.KeyValue) error {
	return db.extendMsgDatabase.DeleteReactionExtendMsgSet(ctx, conversationID, sessionType, clientMsgID, msgFirstModifyTime, db.extendMsgSetModel.Pb2Model(reactionExtensions))
}
