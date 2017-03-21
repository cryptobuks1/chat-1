package controller

import (
	"xsbPro/chatDispatcher/dispatcher"
	"xsbPro/chatDispatcher/lua"
	"xsbPro/chatDispatcher/resource"
	"xsbPro/common"
	"xsbPro/log"

	"encoding/json"
	"sync"
	"time"

	nsq "github.com/nsqio/go-nsq"
	"github.com/ssor/config"
	"gopkg.in/mgo.v2/bson"
)

var (
	lookupdPollInterval = 15 * time.Second
	nsqChannel          = "chatdispatcher"
	conf                config.IConfigInfo

	waitForDbConnection = sync.Mutex{}
)

// Init init resources
func Init(c config.IConfigInfo) {
	conf = c

	resource.Init(conf)

	dispatcher.Init(conf, resource.Redis_instance.DoScript, resource.Redis_instance.RedisDo)

	//当支部发生变化时的处理
	//1. 新添加了支部    -> 添加新支部信息,更新支部人员信息,更新人员和支部关系,只需分配节点
	//3. 删除支部       -> 清理数据,通知节点
	//4. 支部人员信息更新 ->  更新支部人员信息,更新人员和支部关系,通知节点
	//5. 人员信息增加    -> 更新人员信息,更新人员和支部关系
	//6. 人员信息更新    -> 更新人员信息
	//7. 人员删除       -> 移除人员信息,更新人员和支部关系

	startNsqConsumer(conf.Get("nsqHost").(string), common.NSQ_topic_group, nsq.HandlerFunc(updateGroup))
	startNsqConsumer(conf.Get("nsqHost").(string), common.NSQ_topic_user, nsq.HandlerFunc(updateUser))
	startNsqConsumer(conf.Get("nsqHost").(string), common.NSQ_topic_users_of_group_update, nsq.HandlerFunc(updateUsersOfGroup))

}

func init() {

}

func updateUsersOfGroup(msg *nsq.Message) error {
	type InnerSyncMessage struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	var ins InnerSyncMessage
	err := json.Unmarshal(msg.Body, &ins)
	if err != nil {
		log.SysF("updateUsersOfGroup err: %s", err)
		return err
	}

	err = updateUsersOfGroupToDB(conf.Get("dbName").(string), ins.Data, resource.Redis_instance.RedisDoMulti)
	if err != nil {
		log.SysF("updateUsersOfGroup err: %s", err)
		return err
	}
	//notify node to remove group
	err = dispatcher.NotifyNodeDataRefresh(dispatcher.Datarefresh_update_users_of_group, ins.Data, resource.Redis_instance.RedisDo)
	if err != nil {
		log.SysF("updateUsersOfGroup err: %s", err)
		return err
	}
	return nil
}

func updateUsersOfGroupToDB(dbName, group string, cmdsExecutor func(*common.RedisCommands) error) error {
	waitForDbConnection.Lock()
	defer waitForDbConnection.Unlock()

	session, err := resource.Mongo_pool.GetSession()
	defer resource.Mongo_pool.ReturnSession(session, err)
	if err != nil {
		return err
	}
	err = dispatcher.UpdateUsersOfGroup(session, dbName, group, cmdsExecutor)
	if err != nil {
		return err
	}
	return nil
}

func updateUser(msg *nsq.Message) error {
	type InnerSyncMessage struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	var ins InnerSyncMessage
	err := json.Unmarshal(msg.Body, &ins)
	if err != nil {
		log.SysF("update user err: %s", err)
		return err
	}
	switch ins.Type {
	case "add", "update":
		// session := resource.Mongo_pool.GetSession()
		// defer resource.Mongo_pool.ReturnSession(session)
		// err = dispatcher.AddUsers(session, conf.GetDbName(), bson.M{"_id": ins.Data}, resource.Redis_instance.RedisDoMulti)
		err = updateUserToDB(conf.Get("dbName").(string), bson.M{"_id": ins.Data}, resource.Redis_instance.RedisDoMulti)
		if err != nil {
			log.SysF("update user err: %s", err)
			return err
		}
	case "remove":
		err = dispatcher.RemoveUsersFromRedis([]string{ins.Data}, resource.Redis_instance.RedisDoMulti)
		if err != nil {
			log.SysF("update user err: %s", err)
			return err
		}
	}
	return nil
}

func updateUserToDB(dbName string, query interface{}, cmdsExecutor func(*common.RedisCommands) error) error {
	waitForDbConnection.Lock()
	defer waitForDbConnection.Unlock()

	session, err := resource.Mongo_pool.GetSession()
	defer resource.Mongo_pool.ReturnSession(session, err)
	if err != nil {
		return err
	}
	err = dispatcher.AddUsers(session, dbName, query, cmdsExecutor)
	if err != nil {
		return err
	}
	return nil
}

func updateGroup(msg *nsq.Message) error {
	type InnerSyncMessage struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}
	var ins InnerSyncMessage
	err := json.Unmarshal(msg.Body, &ins)
	if err != nil {
		log.SysF("update group err: %s", err)
		return err
	}
	switch ins.Type {
	case "add":
		// session := resource.Mongo_pool.GetSession()
		// defer resource.Mongo_pool.ReturnSession(session)
		// err = dispatcher.AddNewGroup(session, conf.GetDbName(), ins.Data, resource.Redis_instance.RedisDoMulti, resource.Redis_instance.DoScript)
		err = addNewGroupToDB(conf.Get("dbName").(string), ins.Data, resource.Redis_instance.RedisDoMulti, resource.Redis_instance.DoScript)
		if err != nil {
			log.SysF("updateGroup err: %s", err)
			return err
		}
	case "remove":
		err = lua.RemoveGroupFromRedis(ins.Data, resource.Redis_instance.DoScript)
		if err != nil {
			return err
		}
		//notify node to remove group
		err = dispatcher.NotifyNodeDataRefresh(dispatcher.Datarefresh_remove_group, ins.Data, resource.Redis_instance.RedisDo)
		if err != nil {
			log.SysF("updateGroup err: %s", err)
			return err
		}
	case "update": //暂时忽略,当前用不到具体的支部信息
	}
	return nil
}

func addNewGroupToDB(dbName, groupID string, cmdsExecutor func(*common.RedisCommands) error, scriptExecutor dispatcher.ScriptExecutor) error {
	waitForDbConnection.Lock()
	defer waitForDbConnection.Unlock()

	session, err := resource.Mongo_pool.GetSession()
	defer resource.Mongo_pool.ReturnSession(session, err)
	if err != nil {
		return err
	}
	err = dispatcher.AddNewGroup(session, dbName, groupID, cmdsExecutor, scriptExecutor)
	if err != nil {
		return err
	}
	return nil
}

func startNsqConsumer(nsqlookupdAddress, topic string, handler nsq.Handler) {
	config := nsq.NewConfig()
	config.LookupdPollInterval = lookupdPollInterval
	// var err error

	consumer, err := nsq.NewConsumer(topic, nsqChannel, config)
	if err != nil {
		panic(err)
	}

	consumer.AddHandler(handler)

	err = consumer.ConnectToNSQLookupd(nsqlookupdAddress)
	// err = consumer.ConnectToNSQD("127.0.0.1:4150")
	if err != nil {
		panic("ConnectToNSQLookupd failed: " + err.Error())
	}
}