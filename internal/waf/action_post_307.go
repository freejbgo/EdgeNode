package waf

import (
	"github.com/TeaOSLab/EdgeNode/internal/remotelogs"
	"github.com/TeaOSLab/EdgeNode/internal/utils"
	"github.com/TeaOSLab/EdgeNode/internal/waf/requests"
	"github.com/iwind/TeaGo/maps"
	"net/http"
	"time"
)

type Post307Action struct {
	Life int32 `yaml:"life" json:"life"`

	BaseAction
}

func (this *Post307Action) Init(waf *WAF) error {
	return nil
}

func (this *Post307Action) Code() string {
	return ActionPost307
}

func (this *Post307Action) IsAttack() bool {
	return false
}

func (this *Post307Action) WillChange() bool {
	return true
}

func (this *Post307Action) Perform(waf *WAF, group *RuleGroup, set *RuleSet, request requests.Request, writer http.ResponseWriter) (allow bool) {
	var cookieName = "WAF_VALIDATOR_ID"

	// 仅限于POST
	if request.WAFRaw().Method != http.MethodPost {
		return true
	}

	// 是否已经在白名单中
	if SharedIPWhiteList.Contains("set:"+set.Id, request.WAFRemoteIP()) {
		return true
	}

	// 判断是否有Cookie
	cookie, err := request.WAFRaw().Cookie(cookieName)
	if err == nil && cookie != nil {
		m, err := utils.SimpleDecryptMap(cookie.Value)
		if err == nil && m.GetString("remoteIP") == request.WAFRemoteIP() && time.Now().Unix() < m.GetInt64("timestamp")+10 {
			var life = m.GetInt64("life")
			if life <= 0 {
				life = 600 // 默认10分钟
			}
			var setId = m.GetString("setId")
			SharedIPWhiteList.Add("set:"+setId, request.WAFRemoteIP(), time.Now().Unix()+life)
			return true
		}
	}

	var m = maps.Map{
		"timestamp": time.Now().Unix(),
		"life":      this.Life,
		"setId":     set.Id,
		"remoteIP":  request.WAFRemoteIP(),
	}
	info, err := utils.SimpleEncryptMap(m)
	if err != nil {
		remotelogs.Error("WAF_POST_302_ACTION", "encode info failed: "+err.Error())
		return true
	}

	// 设置Cookie
	http.SetCookie(writer, &http.Cookie{
		Name:   cookieName,
		Path:   "/",
		MaxAge: 10,
		Value:  info,
	})

	http.Redirect(writer, request.WAFRaw(), request.WAFRaw().URL.String(), http.StatusTemporaryRedirect)

	// 关闭连接
	_ = this.CloseConn(writer)

	return true
}
