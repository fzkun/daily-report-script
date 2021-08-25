package repo

import (
	"InNurse-Service/lmodel"
	"fmt"
	"git.gdqlyt.com.cn/go/base/beego/bmodel"
	"git.gdqlyt.com.cn/go/base/collection"
	"git.gdqlyt.com.cn/go/chis-pub/models"
	"git.gdqlyt.com.cn/go/chis-pub/rpcservice"
	"git.gdqlyt.com.cn/go/chis/chiscontext"
	"git.gdqlyt.com.cn/go/chis/chislog"
	"git.gdqlyt.com.cn/go/chis/db"
	chisrepo "git.gdqlyt.com.cn/go/chis/repo"
	"git.gdqlyt.com.cn/go/xorm"
	"github.com/astaxie/beego/logs"
	"github.com/minio/minio-go/pkg/set"
	"strconv"
)

type OrderRepo struct {
	chisrepo.BaseRepo
	*OrderHandleRepo
}

func NewOrderRepo(ctx *chiscontext.ChisContext) *OrderRepo {
	repo := new(OrderRepo)
	repo.ChisCtx = ctx
	repo.OrderHandleRepo = NewOrderHandleRepo(ctx)
	return repo
}

func (repo *OrderRepo) GetContext() *chiscontext.ChisContext {
	return repo.ChisCtx
}

/**
 * 批量确认医嘱
 */
func (repo *OrderRepo) ConfirmExecn(gids []string, param ...string) error {
	return With(repo, func(session *xorm.Session) (err0 error) {
		var (
			outhos     bool
			now        = bmodel.NewNowLocalTime()
			nurseid    = param[0]
			migrations = new([]*models.ChInPatMigrate)
			groups     = new([]*models.ChInorderGroup)
			valideds   = new([]*models.ChInorderGroup)
			execs      = new([]models.ChOrderExec)
			pexecs     = new([]models.ChOrderExec)
		)

		// 只找`已保存`的相关医嘱
		io, ig := models.Table_ChInOrder, models.Table_ChInOrderGroup
		err := session.
			Join("LEFT", io, fmt.Sprintf("%s.id = orderid", io)).
			In(fmt.Sprintf("%s.id", ig), gids).
			And(fmt.Sprintf("%s.status = ?", io), lmodel.OrdSaved).
			Find(groups)
		if err != nil || len(*groups) == 0 {
			return fmt.Errorf("医嘱未保存或医嘱已确认，%v", err)
		}

		// 生成执行计划和特殊医嘱事务记录
		for _, g := range *groups {
			// 生成执行计划
			if collection.Contain(g.Transactionid, SpecTxs) {
				g.Firstnum = 1
				g.Performid = lmodel.NurObjOther
			}
			es, err := MakeOrderexec(session, g)
			if err != nil {
				msg := "txid：%s，performid：%s，groupid: %s，"
				err0 = lmodel.AppendErr(fmt.Errorf(msg, g.Transactionid, g.Performid, g.Id), err, err0)
				continue
			}

			// 裁减临嘱执行计划
			if g.Orderkind == lmodel.ShortTerm {
				es = es[:1]
				es[0].Orderexecdt = now
			}

			// 处理特殊医嘱
			if collection.Contain(g.Transactionid, SpecTxs) {
				if g.Transactionid == lmodel.NurobjOut {
					outhos = true
					*execs = (*execs)[:0]
					*groups = (*groups)[:0]
					*valideds = (*valideds)[:0]
					*migrations = (*migrations)[:0]
				}
				switch g.Transactionid {
				case lmodel.NurobjOut, lmodel.NurobjDept, lmodel.NurobjBed:
					m := &models.ChInPatMigrate{
						Nurseid:       nurseid,
						Ordergroupid:  g.Id,
						Status:        lmodel.MigConfirmed,
						Invisitid:     g.Visitid,
						Orderid:       g.Orderid,
						Transactionid: g.Transactionid,
						Doctorid:      g.Doctorid,
						Execdt:        now,
					}
					*migrations = append(*migrations, m)
				}
				*pexecs = append(*pexecs, es...)
				*valideds = append(*valideds, g)
				continue
			}

			*execs = append(*execs, es...)
			*valideds = append(*valideds, g)
		}

		if len(*valideds) == 0 {
			return fmt.Errorf("无法生成有效的执行计划或特殊医嘱事务，%v", err0)
		}

		// 如果有出院医嘱则停止此病人其它所有医嘱
		if outhos {
			outord := (*valideds)[0]
			err = session.
				And("visitid = ?", outord.Visitid).
				And("id != ?", (*valideds)[0].Id).
				And("status != ?", lmodel.OrdStoped).
				And("orderkind like '%0'").
				And("deleteflag != '1'").
				Find(groups)
			if err != nil {
				return fmt.Errorf("查找尚未停止的住院医嘱失败，%v", err)
			}
			if len(*groups) != 0 {
				gids = gids[:0]
				for _, g := range *groups {
					gids = append(gids, g.Id)
				}

				param = append(param, outord.Doctorid, outord.Doctorname, lmodel.ForceStop)
				err = NewOrderRepo(repo.ChisCtx).ConfirmStop(gids, param...)
				if err != nil {
					err0 = lmodel.AppendErr(err, err0)
				}
			}
		}

		// 更新特殊医嘱事务记录
		for _, m := range *migrations {
			// 检查事务记录是否存在
			get, err := session.Where("ordergroupid = ?", m.Ordergroupid).Get(models.ChInPatMigrate{})
			if !get || err != nil {
				m.BaseModel = *lmodel.NewBaseModel(now)
				one, err := session.InsertOne(m)
				if one == 0 || err != nil {
					err0 = lmodel.AppendErr(fmt.Errorf("新增特殊医嘱事务记录失败，groupid：%s", m.Ordergroupid), err, err0)
				}
			} else {
				session.EnableVersion(false)
				update, err := session.Where("ordergroupid = ?", m.Ordergroupid).Update(m)
				if update == 0 || err != nil {
					err0 = lmodel.AppendErr(fmt.Errorf("更新特殊医嘱事务记录失败，groupid: %s", m.Ordergroupid), err, err0)
				}
			}
		}

		// 发送药品药单、账单
		elen := len(*execs)
		if elen != 0 {
			// SendPharmacy 在内部会过滤执行失败的执行计划
			err = SendPharmacy(repo.ChisCtx, execs, (*valideds)[0].Orderkind, true)
			if err != nil {
				err0 = lmodel.AppendErr(err, err0)
			}

			if elen != len(*execs) {
				err0 = lmodel.AppendErr(fmt.Errorf("所选部分药品无法生成药单"), err0)
			}

			// 过滤失败医嘱
			for i := 0; i < len(*valideds); {
				if collection.Contain((*valideds)[i].Transactionid, SpecTxs) {
					i++
					continue
				}
				func() {
					for _, e := range *execs {
						if (*valideds)[i].Id == e.Ordersgroudid {
							i++
							return
						}
					}
					*valideds = append((*valideds)[:i], (*valideds)[i+1:]...)
				}()
			}
		}

		// 发送非处方医嘱的账单
		if len(*pexecs) != 0 {
			err = SendCheques(repo.ChisCtx, pexecs)
			if err != nil {
				err0 = lmodel.AppendErr(err, err0)
			}
		}

		// 插入执行计划
		*execs = append(*execs, *pexecs...)
		if len(*execs) != 0 {
			multi, err := session.InsertMulti(execs)
			if multi == 0 || err != nil {
				err0 = lmodel.AppendErr(fmt.Errorf("multi: %d", multi), err, err0)
			}
		}

		// 修改医嘱状态
		err = SendGroups(repo.ChisCtx, lmodel.OrderConfirm, valideds, param...)
		if err != nil {
			err0 = lmodel.AppendErr(err, err0)
		}

		// 生成可能的皮试项目
		err = NewSkintestRepo(repo.ChisCtx).NewSkintests(valideds)
		if err != nil {
			err0 = lmodel.AppendErr(err, err0)
		}

		// 发送 redis 消息
		err = PublishNursing(db.Message_Innurse_Confirm, repo.ChisCtx, valideds)
		if err != nil {
			logs.Error("广播确认医嘱消息失败", err)
		}

		return
	}, true)
}

/**
 * 批量取消确认医嘱
 */
func (repo *OrderRepo) CancelConfirmn(gids []string, param ...string) error {
	type _InorderGroup struct {
		*models.ChInorderGroup `xorm:"extends"`
		Ecs                    int `xorm:"ecs"`
	}
	return With(repo, func(session *xorm.Session) (err0 error) {
		var (
			method  = rpcservice.InMedicalService.CancelPharmacy
			groups  = new([]*models.ChInorderGroup)
			_groups = new([]*_InorderGroup)
		)

		gtb, etb := models.Table_ChInOrderGroup, models.Table_ChOrderExec
		err := session.
			Select(fmt.Sprintf("%s.*, count(%s.id) as ecs", gtb, etb)).
			Join("LEFT", etb, fmt.Sprintf("%s.id = %s.ordersgroudid and %s.deleteflag != '1'", gtb, etb, etb)).
			In(fmt.Sprintf("%s.id", gtb), gids).
			And("status = ?", lmodel.OrdCfmed).
			And(fmt.Sprintf("%s.deleteflag != '1'", gtb)).
			GroupBy(fmt.Sprintf("%s.id", gtb)).
			Find(_groups)
		if err != nil || len(*_groups) == 0 {
			return fmt.Errorf("请确认医嘱存在并已确认，%v", err)
		}

		// 过滤已发药医嘱
		gids = gids[:0]
		for _, group := range *_groups {
			if group.Ecs > 0 {
				err = rpcservice.GInMedicalRpcClient.Call(repo.ChisCtx, method, &group.Id, new(string))
				if err != nil {
					err0 = lmodel.AppendErr(err, err0)
					continue
				}
				gids = append(gids, group.Id)
			}
			*groups = append(*groups, group.ChInorderGroup)
		}

		// 修改医嘱状态
		err = SendGroups(repo.ChisCtx, lmodel.OrderCancel, groups, param...)
		if err != nil {
			return lmodel.AppendErr(fmt.Errorf("部分医嘱修改状态失败"), err, err0)
		}

		// 修改执行计划状态
		if len(gids) > 0 {
			rm, err := session.EnableVersion(false).In("ordersgroudid", gids).Delete(models.ChOrderExec{})
			if rm == 0 || err != nil {
				err0 = lmodel.AppendErr(fmt.Errorf("rm: %d", rm), err0)
			}
		}

		// 发送 redis 消息
		err = PublishNursing(db.Message_Innurse_ConfirmCancel, repo.ChisCtx, groups)
		if err != nil {
			logs.Error("广播取消确认消息失败", err)
		}

		return
	}, true)
}

/**
 * 批量确认停嘱
 */
func (repo *OrderRepo) ConfirmStop(gids []string, param ...string) error {
	return With(repo, func(session *xorm.Session) (err0 error) {
		var (
			nurseid = param[0]
			force   = param[len(param)-1]
			method  = rpcservice.InChargeService.Refund
			groups  = new([]*models.ChInorderGroup)
			reply   = new(models.ChInorderGroup)
		)

		if force != lmodel.ForceStop {
			session.And("status = ?", lmodel.OrdStoping)
		}

		err := session.
			In("id", gids).
			And("deleteflag != '1'").
			Find(groups)
		if err != nil || len(*groups) == 0 {
			return fmt.Errorf("请确认当前医嘱的状态是否正确，%v", err)
		}

		gids = gids[:0]
		for _, g := range *groups {
			// 发送停嘱账单
			if g.Stopdoctorid == "" {
				g.Stopdoctorid = param[2]
				g.Stopdoctorname = param[3]
			}
			err = rpcservice.GInChargeRpcClient.Call(repo.ChisCtx, method, g, reply)
			if err != nil {
				err0 = lmodel.AppendErr(fmt.Errorf("group id: %s, %v", g.Id, err), err0)
			}
			gids = append(gids, g.Id)

			// 处理特殊医嘱
			switch g.Transactionid {
			case lmodel.NurobjOut, lmodel.NurobjDept, lmodel.NurobjBed:
				session.EnableVersion(false)
				update, err := session.
					SetExpr("status", strconv.Itoa(lmodel.MigDeprecate)).
					SetExpr("nurseid", quote(nurseid)).
					And("ordergroupid = ?", g.Id).
					Update(models.ChInPatMigrate{})
				if update == 0 || err != nil {
					err0 = lmodel.AppendErr(fmt.Errorf("更新特殊医嘱事务记录失败，groupid: %s", g.Id), err, err0)
				}
			}
		}

		// DEBUG DATA
		desc := "医生停嘱"
		if force == lmodel.ForceStop {
			desc = "出院强制停嘱"
		}
		logdata := lmodel.RefundLog{Gids: gids}
		if err0 != nil {
			logdata.Err = err0.Error()
		}
		chislog.LogByHospitalCode(repo.ChisCtx, "发送停嘱账单", desc, logdata)

		// 修改医嘱状态
		err = SendGroups(repo.ChisCtx, lmodel.StopConfirm, groups, param...)
		if err != nil {
			err0 = lmodel.AppendErr(err, err0)
		}

		// 发送 redis 消息
		err = PublishNursing(db.Message_Innurse_Stop, repo.ChisCtx, groups)
		if err != nil {
			logs.Error("广播确认停嘱消息失败", err)
		}

		return
	}, true)
}

/**
 * 批量取消停嘱
 */
func (repo *OrderRepo) CancelStopn(gids []string, param ...string) error {
	return With(repo, func(session *xorm.Session) error {
		groups := new([]*models.ChInorderGroup)

		err := session.
			In("id", gids).
			And("status = ?", lmodel.OrdStoping).
			And("deleteflag != '1'").
			Find(groups)
		if err != nil || len(*groups) == 0 {
			return fmt.Errorf("请确认当前医嘱的状态是否正确，%v", err)
		}

		err = SendGroups(repo.ChisCtx, lmodel.StopCancel, groups, param...)
		if err != nil {
			return fmt.Errorf("部分医嘱修改状态失败，%v", err)
		}

		// 发送 redis 消息
		err = PublishNursing(db.Message_Innurse_StopCancel, repo.ChisCtx, groups)
		if err != nil {
			logs.Error("广播取消停嘱消息失败", err)
		}

		return nil
	})
}

/**
 * 不支持的医嘱操作
 */
func (repo *OrderRepo) UnsupportOrder([]string, ...string) (err error) {
	return fmt.Errorf("该医嘱不支持此操作")
}

/**
 * 列取执行计划
 */
func (repo *OrderRepo) ListExecute(req *lmodel.ExecListReq, resp *[]*models.ChOrderExec) error {
	var session = repo.NewSession()
	defer func() { session.Close() }()

	return session.
		Where("ordersgroudid = ?", req.Groupid).
		And("orderexecdt like ?", req.Execdate+"%").
		And("deleteflag != '1'").
		Asc("orderexecdt").
		Find(resp)
}

/**
 * 执行医嘱
 */
func (repo *OrderRepo) Execute(req *lmodel.ExecuteReq, user *rpcservice.LoginUserInfo) error {
	return With(repo, func(session *xorm.Session) error {
		var (
			now  = bmodel.NewNowLocalTime()
			exec = new(models.ChOrderExec)
		)

		exec.Actualexecdt = now
		exec.Execcomment = req.Comment
		exec.Execuid = user.Userid
		exec.Execuname = user.Username
		exec.State = lmodel.OrderExeced
		update, err := session.EnableVersion(false).ID(req.Execid).Update(exec)
		if update == 0 || err != nil {
			err = fmt.Errorf("找不到相应的执行计划或已执行完成，update：%d，%v", update, err)
		}

		return err
	})
}

/**
 * 更新医嘱组
 */
type inOrderReq struct {
	OrderGroups []models.ChInorderGroup
	Orderid     string
	NurseId     string
	NurseName   string
	DoctorId    string
	DoctorName  string
}

func SendGroups(ctx *chiscontext.ChisContext, action int, gs *[]*models.ChInorderGroup, param ...string) (err error) {
	var (
		sendloop bool
		method   interface{}
		req      = new(inOrderReq)
		success  = new(bool)
	)

	switch action {
	case lmodel.OrderConfirm: // 确认医嘱
		method = rpcservice.InDoctorService.InDoctorOrderConfirm
	case lmodel.OrderCancel: // 取消医嘱
		method = rpcservice.InDoctorService.InDoctorOrderCancelConfirm
	case lmodel.StopConfirm: // 确认停嘱
		method = rpcservice.InDoctorService.InDoctorNurseStopOrder
		sendloop = true
	case lmodel.StopCancel: // 取消停嘱
		method = rpcservice.InDoctorService.InDoctorNurseCancelStopOrder
		sendloop = true
	case lmodel.Deprecate: // 作废医嘱
		return fmt.Errorf("护士不能作废医嘱")
	}
	req.NurseId = param[0]
	req.NurseName = param[1]

	// 停嘱只能循环发送 OrderId
	if sendloop {
		omap := make(map[string]bool)
		valids := new([]*models.ChInorderGroup)
		for _, g := range *gs {
			if !omap[g.Orderid] {
				omap[g.Orderid] = true
				req.Orderid = g.Orderid
				req.DoctorId = g.Stopdoctorid
				req.DoctorName = g.Stopdoctorname
				_err := rpcservice.GInDoctorRpcClient.Call(ctx, method, req, success)
				if _err != nil || !*success {
					err = lmodel.AppendErr(_err, err)
					continue
				}
				*valids = append(*valids, g)
			}
		}
		*gs = *valids
	} else {
		req.OrderGroups = make([]models.ChInorderGroup, len(*gs))
		for i, g := range *gs {
			req.OrderGroups[i] = *g
		}
		err = rpcservice.GInDoctorRpcClient.Call(ctx, method, req, success)
	}
	if err != nil || !*success {
		err = fmt.Errorf("RPC 更新医嘱失败，success：%t，%v", *success, err)
	}
	return
}

/**
 * 发送药品的药单、账单
 */
func SendPharmacy(ctx *chiscontext.ChisContext, execs *[]models.ChOrderExec, orderkind string, isfirst bool) (err0 error) {
	if orderkind == lmodel.LongTerm {
		return sendPharmacy(ctx, execs, orderkind, isfirst)
	}

	// 以一个完整的医嘱为序列处理数据
	var emap = make(map[string]*[]models.ChOrderExec)
	for _, exec := range *execs {
		es, exsit := emap[exec.Ordersid]
		if !exsit {
			es = new([]models.ChOrderExec)
			emap[exec.Ordersid] = es
		}
		*es = append(*es, exec)
	}

	*execs = (*execs)[:0]
	for _, es := range emap {
		err := sendPharmacy(ctx, es, orderkind, isfirst)
		if err != nil {
			err0 = lmodel.AppendErr(err, err0)
			continue
		}
		*execs = append(*execs, *es...)
	}

	return err0
}

func sendPharmacy(ctx *chiscontext.ChisContext, execs *[]models.ChOrderExec, orderkind string, isfirst bool) (err0 error) {
	// DEBUG DATA
	logdata := lmodel.NewPharmacyLog()
	first, kind := "", "长期医嘱"
	if isfirst {
		first = "首次"
	}
	if orderkind == lmodel.ShortTerm {
		kind = "临时医嘱"
	}
	desc := fmt.Sprintf("%s, %s", kind, first)
	defer func() {
		if err0 != nil {
			logdata.Err = err0.Error()
		}
		chislog.LogByHospitalCode(ctx, "处理执行计划药单", desc, logdata)
	}()

	for _, exec := range *execs {
		logdata.Before[exec.Ordersgroudid] = logdata.Before[exec.Ordersgroudid] + 1
	}

	// 发送药单
	var (
		medMeth  interface{}
		failures = new([]models.ChOrderExec)
		failset  = set.NewStringSet()
	)

	switch {
	case orderkind == lmodel.ShortTerm:
		medMeth = rpcservice.InMedicalService.CreateShortPharmaryWest
	case orderkind == lmodel.LongTerm && isfirst:
		medMeth = rpcservice.InMedicalService.FirstExecComfirm
	case orderkind == lmodel.LongTerm:
		medMeth = rpcservice.InMedicalService.OrderExecscreen
	}

	err := rpcservice.GInMedicalRpcClient.Call(ctx, medMeth, execs, failures)
	if err != nil {
		err0 = lmodel.AppendErr(err, err0)
	}

	for _, fail := range *failures {
		failset.Add(fail.Id)
		logdata.Failures[fail.Ordersgroudid] = logdata.Failures[fail.Ordersgroudid] + 1
	}

	// 过滤失败的执行计划
	for i := 0; i < len(*execs); {
		exec := (*execs)[i]
		if failset.Contains(exec.Id) {
			*execs = append((*execs)[:i], (*execs)[i+1:]...)
			continue
		}
		i++
	}

	for _, succ := range *execs {
		logdata.After[succ.Ordersgroudid] = logdata.After[succ.Ordersgroudid] + 1
	}

	// 发送账单
	err = SendCheques(ctx, execs)
	if err != nil {
		err0 = lmodel.AppendErr(err, err0)
	}

	return
}

/**
 * 发送医嘱账单
 */
func SendCheques(ctx *chiscontext.ChisContext, execs *[]models.ChOrderExec) (err0 error) {
	var charMeth = rpcservice.InChargeService.InAccountListDetailInsert
	for _, exec := range *execs {
		err := rpcservice.GInChargeRpcClient.Call(ctx, charMeth, exec, new([]models.ChInAccountListDetail))
		err0 = lmodel.AppendErr(err, err0)
	}

	return err0
}

/**
 * 账单出院召回
 */
func SendRecall(ctx *chiscontext.ChisContext, visitid int32) (recallErr error) {
	var charMeth = rpcservice.InChargeService.CallbackPatient

	err := rpcservice.GInChargeRpcClient.Call(ctx, charMeth, int(visitid), &recallErr)
	if err != nil {
		return err
	}

	return recallErr
}
