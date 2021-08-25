package cron

import (
	"InNurse-Service/lmodel"
	"InNurse-Service/repo"
	"fmt"
	"git.gdqlyt.com.cn/go/base/collection"
	"git.gdqlyt.com.cn/go/chis-pub/models"
	"git.gdqlyt.com.cn/go/chis/chiscontext"
	"git.gdqlyt.com.cn/go/chis/db"
	"git.gdqlyt.com.cn/go/chis/service"
	"git.gdqlyt.com.cn/go/xorm"
	"github.com/astaxie/beego/logs"
	"github.com/go-redis/redis"
	"strings"
	"time"
)

type Cfg struct {
	hosS     []string
	agencyT  []string
	taskName string
	taskDesc string
	lockKey  string
}

type cps func(*chiscontext.ChisContext) error

/**
 * 遍历机构
 */
func ForeachAgency(cfg Cfg, cps cps) (err0 error) {
	defer func() {
		if err0 != nil {
			logs.Error("task fail:", cfg.taskName)
		}
	}()

	if cfg.lockKey != "" {
		rt := db.GetManagerRedisTool()
		lock, err := rt.Lock(cfg.lockKey, 0)
		if err != nil {
			logs.Info("已有其它子服务执行该定时任务，%v，%v", cfg.taskName, err)
			return
		}
		defer func() { _ = lock.Unlock() }()

		_, err = rt.GetString(cfg.lockKey)
		if err != redis.Nil {
			logs.Info("该定时任务今天已执行完成，%v", cfg.taskName)
			return
		}

		_ = rt.Save(cfg.lockKey, cfg.lockKey)
		_ = rt.Expire(cfg.lockKey, time.Hour*12)
	}

	var (
		agencys = service.GetHospitalCodes()
		ce, l   = make(chan error), 0
	)
	// 遍历所有机构
	for _, agency := range agencys {
		if len(cfg.hosS) > 0 && !collection.Contain(agency.Agencycode, cfg.hosS) {
			continue
		}

		if !collection.Contain(agency.Agencytype, cfg.agencyT) {
			continue
		}

		ctx, err := chiscontext.NewWithHospital(agency.Agencycode)
		if err != nil || ctx == nil || ctx.Context == nil {
			err0 = lmodel.AppendErr(err, err0)
			continue
		}

		engine := db.GetHisDB(ctx)
		if engine == nil {
			continue
		}

		err = engine.Ping()
		if err != nil {
			err0 = lmodel.AppendErr(err, err0)
			continue
		}

		l++
		go func(acode string) {
			err := cps(ctx)
			if err != nil {
				err = fmt.Errorf("%s:%s -> %v", acode, cfg.taskName, err)
			}
			ce <- err
		}(agency.Agencycode)
	}

	for i := 0; i < l; i++ {
		select {
		case err := <-ce:
			err0 = lmodel.AppendErr(err, err0)
		}
	}

	return
}

/**
 * DEBUG
 * 批量出院时修复异常医嘱
 */
func DetermineOrder() error {
	cfg := Cfg{
		taskName: "DetermineOrder",
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewOrderRepo(ctx), func(session *xorm.Session) error {
			var v, g = models.Table_ChInVisit, models.Table_ChInOrderGroup
			var groups = new([]*models.ChInorderGroup)
			var err = session.
				Table(g).
				Join("LEFT", v, "visitid = invisitid").
				In("orderkind", lmodel.LongTerm, lmodel.LongDuring).
				NotIn("transactionid", repo.SpecTxs).
				In(v+".status", lmodel.Discharging, lmodel.Settlement, lmodel.Discharged).
				And(g+".status != ?", lmodel.OrdStoped).
				And(g + ".deleteflag != '1'").
				Find(groups)
			if err != nil {
				return err
			}
			if len(*groups) == 0 {
				return nil
			}

			return repo.SendGroups(ctx, lmodel.StopConfirm, groups, "oKLYY91wpSweMEKJ8TzeHrg", "超级管理员")
		})
	})
}

/**
 * 生成住院执行计划，错误则回滚
 */
func OrderExecTimer() error {
	cfg := Cfg{
		taskName: OrderExecSyncer,
		taskDesc: "执行计划自动生成",
		lockKey:  OrderExecSyncer,
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewOrderRepo(ctx), func(session *xorm.Session) (err0 error) {
			execs := new([]models.ChOrderExec)
			groups := new([]*models.ChInorderGroup)

			// 发送药品药单、账单
			err := session.
				In("orderkind", lmodel.LongTerm, lmodel.LongDuring).
				NotIn("transactionid", repo.SpecTxs).
				And("status = ?", lmodel.OrdCfmed).
				And("deleteflag != '1'").
				Find(groups)
			if err != nil {
				err0 = lmodel.AppendErr(err, err0)
			}

			*execs, err = repo.MakeOrderexecMulti(session, *groups)
			if err != nil {
				err0 = lmodel.AppendErr(err, err0)
			}

			srcElen := len(*execs)
			if srcElen != 0 {
				err = repo.SendPharmacy(ctx, execs, lmodel.LongTerm, false)
				if err != nil {
					err0 = lmodel.AppendErr(err, err0)
				}

				if srcElen != len(*execs) {
					err0 = lmodel.AppendErr(fmt.Errorf("部分药品无法生成药单"), err0)
				}

				if len(*execs) != 0 {
					multi, err := session.InsertMulti(execs)
					if multi == 0 || err != nil {
						err0 = lmodel.AppendErr(fmt.Errorf("multi: %d, %v", multi, err), err0)
					}
				}
			}

			*groups = (*groups)[:0]
			*execs = (*execs)[:0]

			// 发送非药品医嘱的账单
			err = session.
				In("orderkind", lmodel.LongTerm, lmodel.LongDuring).
				In("transactionid", repo.SpecTxs).
				And("status = ?", lmodel.OrdCfmed).
				And("deleteflag != '1'").
				Find(groups)
			if err != nil {
				err0 = lmodel.AppendErr(err, err0)
			}

			*execs, err = repo.MakeOrderexecMulti(session, *groups)
			if err != nil {
				err0 = lmodel.AppendErr(err, err0)
			}

			if len(*execs) > 0 {
				err = repo.SendCheques(ctx, execs)
				if err != nil {
					err0 = lmodel.AppendErr(fmt.Errorf("send cheques, %v", err), err0)
				}

				multi, err := session.InsertMulti(execs)
				if multi == 0 || err != nil {
					err0 = lmodel.AppendErr(fmt.Errorf("multi: %d, %v", multi, err), err0)
				}
			}

			return
		}, true)
	})
}

/**
 * 因未知的原因，病人结算后状态有时不会更新
 * 修复已结算病人状态
 */
func PatChequeTimer() error {
	cfg := Cfg{
		taskName: PatChequeSyncer,
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewPatientRepo(ctx), func(session *xorm.Session) error {
			pats := new([]*struct{ Id string })
			iv, bb := models.Table_ChInVisit, models.Table_ChIpBalBsn

			err := session.
				Table(iv).
				Cols(fmt.Sprintf("%s.id", iv)).
				Join("INNER", bb, "invisitid = visitid").
				In("curstate", 30, 40, 50).
				NotIn("status", lmodel.Settlement, lmodel.Discharged).
				Find(pats)
			if err != nil {
				return fmt.Errorf("查找病人失败，%v", err)
			}
			if len(*pats) == 0 {
				return nil
			}

			pat := models.ChInVisit{Status: lmodel.Settlement}
			patids := make([]string, len(*pats))
			for i, p := range *pats {
				patids[i] = p.Id
			}

			session.EnableVersion(false)
			update, err := session.In("id", patids).Update(pat)
			if update == 0 || err != nil {
				return fmt.Errorf("更新已结算病人状态失败，update：%d，%v", update, err)
			}

			return nil
		})
	})
}

/**
 * 因未知的原因，病人入科后有时会无病区
 * 修复病人病区信息
 */
func PatMigrateTimer() error {
	cfg := Cfg{
		taskName: PatMigrageSyncer,
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewPatientRepo(ctx), func(session *xorm.Session) (err error) {
			pats := new([]*models.ChInVisit)
			err = session.
				Cols("id", "bedid").
				And("status = ?", lmodel.Immigration).
				And("wardid = ''").
				And("bedid != ''").
				Find(pats)
			if err != nil {
				return fmt.Errorf("查找病人失败，%v", err)
			}

			if len(*pats) == 0 {
				return
			}

			for _, v := range *pats {
				bed := new(models.ChDictBedInfo)
				get, _err := session.ID(v.Bedid).Cols("deptid", "deptname").Get(bed)
				if !get || _err != nil {
					err = lmodel.AppendErr(fmt.Errorf("查找病床失败，bedid：%s", v.Bedid), _err, err)
					continue
				}

				v.Wardid = bed.Deptid
				v.Wardname = bed.Deptname
				update, _err := session.EnableVersion(false).ID(v.Id).Update(v)
				if update == 0 || _err != nil {
					err = lmodel.AppendErr(fmt.Errorf("更新病人失败，id：%s", v.Bedid), _err, err)
				}
			}

			return
		})
	})
}

/**
 * 因未知的原因，床位状态和住院号有时为空
 * 修复病床状态
 */
func BedRestoreTimer() error {
	cfg := Cfg{
		taskName: BedRestoreSyncer,
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewPatientRepo(ctx), func(session *xorm.Session) error {
			beds := new([]*struct{ Id string })
			err := session.
				Table(models.Table_ChBedInfo).
				Cols("id").
				And("status = ''").
				Find(beds)
			if err != nil {
				return fmt.Errorf("查找病床失败：%v", err)
			}
			if len(*beds) == 0 {
				return nil
			}

			bed := models.ChDictBedInfo{Status: lmodel.Awaiting}
			bedids := make([]string, len(*beds))
			for i, b := range *beds {
				bedids[i] = b.Id
			}

			session.EnableVersion(false)
			update, err := session.In("id", bedids).Update(bed)
			if update == 0 || err != nil {
				return fmt.Errorf("重置床位状态失败，update：%d，%v", update, err)
			}

			return nil
		})
	})
}

/**
 * 手动清除病人表时不会自动刷新病床的状态
 * 重置床位
 */
func BedResetTimer() error {
	cfg := Cfg{
		taskName: BedResetSyncer,
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewPatientRepo(ctx), func(session *xorm.Session) error {
			beds := new([]*struct{ Id string })
			err := session.
				Table(models.Table_ChBedInfo).
				Cols(models.Table_ChBedInfo+".id").
				Join("LEFT", models.Table_ChInVisit, "visitid = invisitid").
				And("visitid != 0").
				And("invisitid is NULL").
				Find(beds)
			if err != nil {
				return fmt.Errorf("查找病床失败：%v", err)
			}

			if len(*beds) == 0 {
				return nil
			}

			bed := models.ChDictBedInfo{Status: lmodel.Awaiting}
			bedids := make([]string, len(*beds))
			for i, b := range *beds {
				bedids[i] = b.Id
			}

			session.EnableVersion(false)
			update, err := session.In("id", bedids).MustCols("visitid").Update(bed)
			if update == 0 || err != nil {
				return fmt.Errorf("重置床位状态失败，update：%d，%v", update, err)
			}

			return nil
		})
	})
}

/**
 * 导入旧血压字段数据到新字段
 */
func BPTimer() error {
	cfg := Cfg{
		taskName: BPSyncer,
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return repo.With(repo.NewSymptomRepo(ctx), func(session *xorm.Session) error {
			symptoms := new([]*models.ChPhySymptom)

			err0 := session.
				Cols("id", "bloodpressure").
				And("bloodpressure != ''").
				And("systolic is null or systolic = ''").
				Find(symptoms)
			if err0 != nil {
				return err0
			}

			for _, symptom := range *symptoms {
				split := strings.Split(symptom.Bloodpressure, "/")
				if len(split) == 2 {
					symptom.Systolic = split[0]
					symptom.Diastolic = split[1]
				}

				update, err := session.EnableVersion(false).ID(symptom.Id).Update(symptom)
				if update == 0 || err != nil {
					err0 = lmodel.AppendErr(err, err0)
				}
			}

			return err0
		})
	})
}

/**
 * 同步数据库
 */
func SyncRepo() error {
	cfg := Cfg{
		taskName: "SyncRepo",
		agencyT:  []string{TypeJDS, TypeGLJYY},
	}
	return ForeachAgency(cfg, func(ctx *chiscontext.ChisContext) error {
		return db.GetHisDB(ctx).Sync2(
			//models.ChInVisit{},
			//models.ChPhySymptom{},
			//models.ChOrderExec{},
			//models.ChNursingCases{},
			//models.ChInAccountListDetail{},
			//models.ChInorder{},
			//models.ChInorderGroup{},
			//models.ChInorderDetail{},
			//models.ChNurcasesExtf{},
			//models.ChNurcasesExtv{},
			models.ChPhySymptom{},
		)
	})
}
