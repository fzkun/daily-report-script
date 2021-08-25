const fs = require('fs')
const pwd = process.cwd()
const path = require('path')
const request = require('request')
const logData = process.env.LOG_DATA
const config = require(path.join(pwd, 'config.json'))
let defaultHeaders = {
    Host: 'aliyun31887308.x3china.com',
    Connection: 'keep-alive',
    'Cache-Control': 'max-age=0',
    'sec-ch-ua':
        '" Not;A Brand";v="99", "Google Chrome";v="91", "Chromium";v="91"',
    'sec-ch-ua-mobile': '?0',
    'Upgrade-Insecure-Requests': '1',
    'User-Agent':
        'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36',
    Accept:
        'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.9,application/json, text/javascript, */*; q=0.01',
    'Accept-Language': 'zh-CN,zh;q=0.9,zh-TW;q=0.8,en;q=0.7',
}

fs.readFile(`${pwd}/${logData}.txt`, async (err, data) => {
    // 读取失败
    if (err) throw err
    // 读取成功
    try {
        let commitGroup = new Set()
        formatData(data)
            .forEach(it => {
                it.content = it.content.toLowerCase()
                if (it.content === 'no message') {
                    return
                }
                if (it.content === 'update') {
                    it.content = '更新项目，优化代码'
                }
                let gp = commitGroup[it.date]
                gp = Object.assign([], gp)
                gp.push(it)
                commitGroup[it.date] = gp
            })

        let JSESSIONID = await getJSESSIONID()
        console.log('获取JSESSIONID成功：', JSESSIONID)
        await login(JSESSIONID)

        for (let key in commitGroup) {
            let commitLogs = commitGroup[key]
            commitLogs
                .map(it => it.content)
                .filter((it, pos, self) => self.indexOf(it) === pos)
                .map((it, pos) => Object.assign(commitLogs[pos], {content: it}))
            if (commitLogs.length === 0) {
                return showError('没有commit记录')
            }
            let commitLog = commitLogs.reduce((acc, curr) => Object.assign(acc, {content: curr.content + '，' + acc.content}))
            submitReport(commitLog.content, JSESSIONID, commitLog.date)
        }
    } catch (err) {
        showError(err)
    }
})


function formatData(data) {
    let dataArr = data
        .toString()
        .trim()
        .split('\n')
        .filter((i) => i && ['c', 'A', 'D', ' '].includes(i[0]))
    let result = []
    dataArr.forEach((i, index) => {
        let pos = Math.ceil((index + 1) / 4) - 1
        result[pos] = Object.assign(
            {},
            result[pos]
        )
        switch (i[0]) {
            case 'c':
                result[pos].commitId = i.split(' ')[1]
                break
            case 'A':
                result[pos].author = i.split(' ')[1]
                break
            case 'D':
                result[pos].date = i.split('   ')[1]
                break
            case ' ':
                result[pos].content = i.split('    ')[1]
                break
        }
    })
    return result
}

function getJSESSIONID() {
    return new Promise((resolve, reject) => {
        let headers = Object.assign(defaultHeaders, {
            'Sec-Fetch-Mode': 'navigate',
            'Sec-Fetch-Site': 'none',
            'Sec-Fetch-Dest': 'document',
            'Sec-Fetch-User': '?1'
        })
        request.get(
            {
                url: 'https://aliyun31887308.x3china.com/',
                headers,
            },
            (error, response, body) => {
                // 回调
                if (error) {
                    showError(error)
                } else {
                    // 获取JSESSIONID
                    resolve(response.headers['set-cookie'][0].split(';')[0].split('=')[1])
                }
            }
        )
    })
}

function login(JSESSIONID) {
    return new Promise((resolve, reject) => {
        let headers = Object.assign(defaultHeaders, {
            'X-Requested-With': 'XMLHttpRequest',
            'Content-Type': 'application/x-www-form-urlencoded; charset=UTF-8',
            'Origin': 'https://aliyun31887308.x3china.com',
            'Sec-Fetch-Site': 'same-origin',
            'Sec-Fetch-Mode': 'cors',
            'Sec-Fetch-Dest': 'empty',
            Cookie:
                'activeTitle=%E6%88%91%E7%9A%84%E6%97%A5%E6%8A%A5; JSESSIONID=' +
                JSESSIONID,
        })
        let form = {
            language: '',
            loginName: config.loginName,
            loginPwd: config.loginPwd
        }
        request.post(
            {
                url: 'https://aliyun31887308.x3china.com/account/ajaxlogin',
                headers,
                form
            },
            (error, response, body) => {
                // 回调
                !body.errorMessage && resolve()
            }
        )
    })
}

function submitReport(commitLog, JSESSIONID, reportDate) {
    console.log(reportDate + '日报对应的commit记录:', commitLog.content);
    let headers = Object.assign(defaultHeaders, {
        'Origin': 'https://aliyun31887308.x3china.com',
        'Sec-Fetch-Dest': 'document',
        'Sec-Fetch-User': '?1',
        'Sec-Fetch-Mode': 'navigate',
        'Sec-Fetch-Site': 'same-origin',
        'Content-Type': 'application/x-www-form-urlencoded',
        'Referer': 'https://aliyun31887308.x3china.com/dailyReport/writeReport?historyTime=' + reportDate, // 必填，后端有做校验
        Cookie:
            'activeTitle=%E6%88%91%E7%9A%84%E6%97%A5%E6%8A%A5; JSESSIONID=' +
            JSESSIONID,
    })
    let form = {
        officeStartHours: '8',
        officeStartMinutes: '0',
        officeEndHours: '17',
        officeEndMinutes: '30',
        id: '',
        lessOneTr: '',
        reportDate,
        'dailyTasks[0].workdayCostTime': '7.50',
        'dailyTasks[0].workProject.id': '43',
        'dailyTasks[0].projectType.id': '4',
        'dailyTasks[0].dept.id': '15',
        'dailyTasks[0].taskDesc': commitLog.content,
        'dailyTasks[0].remark': '',
        'dailyTasks[0].status': 'Submited',
        'dailyTasks[0].id': '',
        'dailyTasks[1].workdayCostTime': '',
        'dailyTasks[1].workProject.id': '43',
        'dailyTasks[1].projectType.id': '1',
        'dailyTasks[1].dept.id': '15',
        'dailyTasks[1].taskDesc': '',
        'dailyTasks[1].remark': '',
        'dailyTasks[1].status': 'Submited',
        'dailyTasks[1].id': '',
        'dailyTasks[2].workdayCostTime': '',
        'dailyTasks[2].workProject.id': '43',
        'dailyTasks[2].projectType.id': '1',
        'dailyTasks[2].dept.id': '15',
        'dailyTasks[2].taskDesc': '',
        'dailyTasks[2].remark': '',
        'dailyTasks[2].status': 'Submited',
        'dailyTasks[2].id': '',
        'dailyTasks[3].workdayCostTime': '',
        'dailyTasks[3].workProject.id': '43',
        'dailyTasks[3].projectType.id': '1',
        'dailyTasks[3].dept.id': '15',
        'dailyTasks[3].taskDesc': '',
        'dailyTasks[3].remark': '',
        'dailyTasks[3].status': 'Submited',
        'dailyTasks[3].id': ''
    }
    request.post(
        {
            url: 'https://aliyun31887308.x3china.com/dailyReport/submitReport',
            headers,
            form
        },
        callback
    )

    function callback(error, response, body) {
        if (error) return showError(error)
        if (!body) {
            showError('执行失败，当天日报已提交(确认中)，或被退回')
        } else {
            console.log(reportDate + '提交成功');
        }
    }
}

function getToday() {
    let date = new Date()
    let nowMonth = date.getMonth() + 1
    let strDate = date.getDate()
    let seperator = '-'
    if (nowMonth >= 1 && nowMonth <= 9) {
        nowMonth = '0' + nowMonth
    }
    if (strDate >= 0 && strDate <= 9) {
        strDate = '0' + strDate
    }
    return date.getFullYear() + seperator + nowMonth + seperator + strDate
}

function showError(text) {
    console.log(text)
    setInterval(function () {
        console.log("timer that keeps nodejs processing running");
    }, 1000 * 60 * 60)
}
