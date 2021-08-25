package hismq

import (
	"InNurse-Service/cache"
	"encoding/json"
	"fmt"
	"git.gdqlyt.com.cn/go/chis/chiscontext"

	"InNurse-Service/lmodel"
	"InNurse-Service/repo"
	"git.gdqlyt.com.cn/go/chis-pub/models"
	"git.gdqlyt.com.cn/go/chis-pub/rpcservice"
	"git.gdqlyt.com.cn/go/chis/db"
	"github.com/astaxie/beego/logs"
)

/**
 * 新增、修改、删除医嘱
 */
func SubscribeOrder() {
	db.RedisSubscribe(PromptOrder(db.WsMessage_Innur_Neworder, "有新增的医嘱"), db.Message_Indoctor_NewOrder)
	db.RedisSubscribe(PromptOrder(db.WsMessage_Innur_Modorder, "有修改的医嘱"), db.Message_Indoctor_EditOrder)
}

/**
 * 发送医嘱的 web-socket 消息
 */
func PromptOrder(key db.WsMessageKey, prompt string) db.SubFunc {
	return func(msg db.PubSubMsg) error {
		logs.Debug("Received Message", msg.Channel)
		var (
			data = new(lmodel.Prompt)
			resp = new([]rpcservice.InDoctorInorderRsp)
			err  = json.Unmarshal([]byte(msg.Content), resp)
		)

		if err != nil || len(*resp) == 0 {
			return err
		}

		// 检查是否有新增皮试项目
		var (
			session = db.GetHisDB(msg.ChisCtx).NewSession()
			testids = new([]string)
		)
		defer func() { session.Close() }()

		for _, o := range *resp {
			for _, g := range o.Groups {
				for _, d := range g.Detail {
					if d.Skintestflag == "1" {
						*testids = append(*testids, d.Id)
					}
				}
			}
		}

		count, err := session.Cols("id").In("presdetailid", *testids).Count(&models.ChNurSkinTest{})
		if err != nil {
			return fmt.Errorf("查找皮试项目失败，%v", err)
		}

		data.Patname = (*resp)[0].Inorder.Patname
		data.Deptid = (*resp)[0].Inorder.Deptid
		data.Visitid = (*resp)[0].Inorder.Visitid
		data.Orderkind = (*resp)[0].Inorder.Orderkind
		if int(count) < len(*testids) {
			data.Prompt = "有新增的皮试项目"
			_, err = sendWs(msg.ChisCtx, db.WsMessage_Innur_Skintest, data)
			if err != nil {
				return err
			}
		}

		data.Prompt = prompt
		_, err = sendWs(msg.ChisCtx, key, data)
		return err
	}
}

/**
 * 接收特殊医嘱、注释医嘱
 */
func SubsribeNewSpecOrder() {
	db.RedisSubscribe(func(msg db.PubSubMsg) error {
		var (
			resp = new(rpcservice.InOrderNewRsp)
			err  = json.Unmarshal([]byte(msg.Content), resp)
		)
		logs.Debug("Received Message", msg.Channel, resp.RspType)

		if err != nil {
			return err
		}

		var (
			key    db.WsMessageKey
			reactg rpcservice.InDoctorInorderRsp
			tarid  string
			txid   string
		)

		switch resp.RspType {
		case lmodel.OutHos:
			key = db.WsMessage_Innur_NewOuthos
			txid = lmodel.NurobjOut
			reactg = resp.OutHos.OutOrder
		case lmodel.ChgDept:
			key = db.WsMessage_Innur_NewChgDept
			txid = lmodel.NurobjDept
			reactg = resp.ChgDept.OutOrder
			tarid = resp.ChgDept.Chgdeptid
		case lmodel.ChgBed:
			key = db.WsMessage_Innur_NewChgBed
			txid = lmodel.NurobjBed
			reactg = resp.ChgBed.OutOrder
			tarid = resp.ChgBed.Chgbedid
		default:
			return fmt.Errorf("unknow RspType %d", resp.RspType)
		}

		var groups = make([]*models.ChInorderGroup, len(reactg.Groups))
		for i, g := range reactg.Groups {
			g.Group.Transactionid = txid
			groups[i] = &g.Group
		}

		err = repo.NewPatientRepo(msg.ChisCtx).RecMigration(groups, tarid, resp.OutHos.Outdate)
		if err != nil {
			return fmt.Errorf("记录特殊医嘱 `%d` 失败，%v", resp.RspType, err)
		}

		var data = new(lmodel.Prompt)
		data.Patname = reactg.Inorder.Patname
		data.Deptid = reactg.Inorder.Deptid
		data.Visitid = reactg.Inorder.Visitid
		data.Orderkind = lmodel.ShortTerm
		_, err = sendWs(msg.ChisCtx, key, data)
		return err
	}, db.Message_Indoctor_NewSpecOrder)
}

func sendWs(ctx *chiscontext.ChisContext, key db.WsMessageKey, data *lmodel.Prompt) (int64, error) {
	var bmap = cache.BedcodeMap
	bedcode, err := bmap.Lookup(data.Visitid, repo.NewPatientRepo(ctx).GetBed)
	if err != nil || bedcode == "" {
		logs.Warning(err)
	}

	data.Bedcode = bedcode
	return db.GetHisRedisTool(ctx).PublishWebSocket(key, data, lmodel.WsFilter)
}
