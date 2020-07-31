package confgov2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/douyu/juno/internal/pkg/service/agent"
	"github.com/douyu/juno/internal/pkg/service/appevent"
	"github.com/douyu/juno/internal/pkg/service/clientproxy"
	"github.com/douyu/juno/internal/pkg/service/configresource"
	"github.com/douyu/juno/internal/pkg/service/openauth"
	"github.com/douyu/juno/internal/pkg/service/resource"
	"github.com/douyu/juno/internal/pkg/service/user"
	"github.com/douyu/juno/pkg/cfg"
	"github.com/douyu/juno/pkg/errorconst"
	"github.com/douyu/juno/pkg/model/db"
	"github.com/douyu/juno/pkg/model/view"
	"github.com/douyu/juno/pkg/util"
	"github.com/douyu/jupiter/pkg/xlog"
	"github.com/go-resty/resty/v2"
	"github.com/jinzhu/gorm"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// QueryAgentUsedStatus ..
	queryAgentUsedStatus = "/api/v1/conf/command_line/status"
)

func List(param view.ReqListConfig) (resp view.RespListConfig, err error) {
	var app db.AppInfo

	resp = make(view.RespListConfig, 0)
	list := make([]db.Configuration, 0)

	err = mysql.Where("app_name = ?", param.AppName).First(&app).Error
	if err != nil {
		return resp, err
	}

	err = mysql.Select("id, aid, name, format, env, zone, created_at, updated_at, published_at").
		Where("aid = ?", app.Aid).
		Where("env = ?", param.Env).
		Find(&list).Error

	for _, item := range list {
		resp = append(resp, view.RespListConfigItem{
			ID:          item.ID,
			AID:         item.AID,
			Name:        item.Name,
			Format:      item.Format,
			Env:         item.Env,
			Zone:        item.Zone,
			CreatedAt:   item.CreatedAt,
			UpdatedAt:   item.UpdatedAt,
			DeletedAt:   item.DeletedAt,
			PublishedAt: item.PublishedAt,
		})
	}

	return
}

func Detail(param view.ReqDetailConfig) (resp view.RespDetailConfig, err error) {
	configuration := db.Configuration{}
	err = mysql.Where("id = ?", param.ID).First(&configuration).Error
	if err != nil {
		return
	}
	resp = view.RespDetailConfig{
		ID:          configuration.ID,
		AID:         configuration.AID,
		Name:        configuration.Name,
		Content:     configuration.Content,
		Format:      configuration.Format,
		Env:         configuration.Env,
		Zone:        configuration.Zone,
		CreatedAt:   configuration.CreatedAt,
		UpdatedAt:   configuration.UpdatedAt,
		PublishedAt: configuration.PublishedAt,
	}
	return
}

// Create ..
func Create(param view.ReqCreateConfig) (resp view.RespDetailConfig, err error) {
	var app db.AppInfo
	var appNode db.AppNode

	// 验证应用是否存在
	err = mysql.Where("app_name = ?", param.AppName).First(&app).Error
	if err != nil {
		return
	}

	// 验证Zone-env是否存在
	err = mysql.Where("env = ? and zone_code = ?", param.Env, param.Zone).First(&appNode).Error
	if err != nil {
		if gorm.IsRecordNotFoundError(err) {
			err = fmt.Errorf("该应用不存在该机房-环境")
		}
		return
	}

	configuration := db.Configuration{
		AID:    uint(app.Aid),
		Name:   param.FileName, // 不带后缀
		Format: string(param.Format),
		Env:    param.Env,
		Zone:   param.Zone,
	}

	tx := mysql.Begin()
	{
		// check if name exists
		exists := 0
		err = tx.Model(&db.Configuration{}).Where("aid = ?", app.Aid).
			Where("env = ?", param.Env).
			Where("name = ?", param.FileName).
			Where("format = ?", param.Format).
			Count(&exists).Error
		if err != nil {
			tx.Rollback()
			return
		}

		if exists != 0 {
			tx.Rollback()
			return resp, fmt.Errorf("已存在同名配置")
		}

		err = tx.Create(&configuration).Error
		if err != nil {
			tx.Rollback()
			return
		}
	}

	err = tx.Commit().Error
	if err != nil {
		tx.Rollback()
		return
	}

	resp = view.RespDetailConfig{
		ID:          configuration.ID,
		AID:         configuration.AID,
		Name:        configuration.Name,
		Content:     configuration.Content,
		Format:      configuration.Format,
		Env:         configuration.Env,
		Zone:        configuration.Zone,
		CreatedAt:   configuration.CreatedAt,
		UpdatedAt:   configuration.UpdatedAt,
		PublishedAt: configuration.PublishedAt,
	}

	return
}

// Update ..
func Update(c echo.Context, param view.ReqUpdateConfig) (err error) {
	configuration := db.Configuration{}
	err = mysql.Where("id = ?", param.ID).First(&configuration).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return errorconst.ParamConfigNotExists.Error()
		}
		return err
	}

	newContent := configresource.FillConfigResource(param.Content)
	oldContent := configresource.FillConfigResource(configuration.Content)

	// 计算本次版本号
	version := util.Md5Str(newContent)
	if util.Md5Str(oldContent) == version {
		return fmt.Errorf("保存失败，本次无更新")
	}

	history := db.ConfigurationHistory{
		ConfigurationID: configuration.ID,
		ChangeLog:       param.Message,
		Content:         param.Content,
		Version:         version,
	}

	// 授权对象
	ok, accessToken := openauth.OpenAuthAccessToken(c)
	if ok {
		// 获取Open Auth信息
		history.AccessTokenID = accessToken.ID
	} else {
		u := user.GetUser(c)
		if u != nil {
			history.UID = uint(u.Uid)
		} else {
			err = fmt.Errorf("无法获取授权对象信息")
			return err
		}
	}

	// 配置/资源 关联关系
	resourceValues, err := ParseConfigResourceValuesFromConfig(history)
	if err != nil {
		return
	}

	tx := mysql.Begin()
	{
		err = tx.Where("version=?", version).Delete(&db.ConfigurationHistory{}).Error
		if err != nil {
			tx.Rollback()
			return err
		}

		// 存历史版本
		err = tx.Save(&history).Error
		if err != nil {
			tx.Rollback()
			return err
		}

		// 存资源配置关联
		for _, value := range resourceValues {
			err = tx.Save(&db.ConfigurationResourceRelation{
				ConfigurationHistoryID: history.ID,
				ConfigResourceValueID:  value.ID,
			}).Error
			if err != nil {
				tx.Rollback()
				return
			}
		}

		configuration.Content = param.Content
		configuration.Version = version
		err = tx.Save(&configuration).Error
		if err != nil {
			tx.Rollback()
			return err
		}

	}

	err = tx.Commit().Error
	if err != nil {
		tx.Rollback()
		return err
	}

	return
}

func ParseConfigResourceValuesFromConfig(history db.ConfigurationHistory) ([]db.ConfigResourceValue, error) {
	var resourceValues []db.ConfigResourceValue
	resources := configresource.ParseResourceFromConfig(history.Content)
	resourceValueIds := make([]uint, 0) // 版本就是资源值ID，全局唯一
	for _, res := range resources {
		resourceValueIds = append(resourceValueIds, res.Version)
	}

	err := mysql.Where("id in (?)", resourceValueIds).Find(&resourceValues).Error
	if err != nil {
		xlog.Error("confgov2.ParseConfigResourceValuesFromConfig", xlog.String("error", "query resource-values failed:"+err.Error()))
		return nil, err
	}

	return resourceValues, nil
}

// Instances ..
func Instances(param view.ReqConfigInstanceList) (resp view.RespConfigInstanceList, err error) {
	// input params
	var (
		env      = param.Env
		zoneCode = param.ZoneCode
	)
	// process
	var (
		configuration db.Configuration
		// configurationHistory db.ConfigurationHistory
		changeLog string
		version   string

		app   db.AppInfo
		nodes []db.AppNode
	)
	// get configuration info
	query := mysql.Where("id=?", param.ConfigurationID).Find(&configuration)
	if query.Error != nil {
		err = query.Error
		return
	}
	// get app info
	if app, err = resource.Resource.GetApp(configuration.AID); err != nil {
		return
	}

	xlog.Debug("Instances", xlog.String("step", "app"), zap.Any("app", app))

	// get all node list
	if nodes, err = resource.Resource.GetAllAppNodeList(db.AppNode{
		Aid:      int(configuration.AID),
		Env:      env,
		ZoneCode: zoneCode,
	}); err != nil {
		return
	}

	xlog.Debug("Instances", xlog.String("step", "nodes"), zap.Any("nodes", nodes))

	filePath := ""

	nodesMap := make(map[string]db.AppNode, 0)

	for _, node := range nodes {
		used := uint(0)
		synced := uint(0)
		takeEffect := uint(0)

		var status db.ConfigurationStatus
		var statusErr error
		status, statusErr = getConfigurationStatus(param.ConfigurationID, node.HostName)
		if statusErr != nil {
			xlog.Error("Instances", xlog.String("step", "nodes"), zap.Any("nodes", nodes), zap.String("statusErr", statusErr.Error()))
			continue
		}

		nodesMap[node.HostName] = node

		filePath = status.ConfigurationPublish.FilePath
		used = status.Used
		synced = status.Synced
		takeEffect = status.TakeEffect

		resp = append(resp, view.RespConfigInstanceItem{
			ConfigurationStatusID: status.ID,
			Env:                   node.Env,
			IP:                    node.IP,
			HostName:              node.HostName,
			DeviceID:              node.DeviceID,
			RegionCode:            node.RegionCode,
			RegionName:            node.RegionName,
			ZoneCode:              node.ZoneCode,
			ZoneName:              node.ZoneName,
			ConfigFilePath:        filePath,
			ConfigFileUsed:        used,
			ConfigFileSynced:      synced,
			ConfigFileTakeEffect:  takeEffect,
			SyncAt:                time.Now(),
			Version:               version,
			ChangeLog:             changeLog,
		})
	}

	// sync used status
	resp, err = syncUsedStatus(nodes, resp, env, zoneCode, filePath)

	// sync publish status
	resp, err = syncPublishStatus(app.AppName, env, zoneCode, configuration, nodesMap, resp)

	// sync take effect status
	resp, err = syncTakeEffectStatus(app.AppName, app.GovernPort, env, zoneCode, configuration, nodesMap, resp)

	return
}

func assemblyJunoAgent(nodes []db.AppNode) []view.JunoAgent {
	res := make([]view.JunoAgent, 0)
	for _, node := range nodes {
		res = append(res, view.JunoAgent{HostName: node.HostName, IPPort: node.IP + fmt.Sprintf(":%d", cfg.Cfg.Agent.Port)})
	}
	return res
}

// Publish ..
func Publish(param view.ReqPublishConfig, user *db.User) (err error) {
	// Complete configuration release logic

	// Get configuration
	var configuration db.Configuration
	query := mysql.Where("id=?", param.ID).Find(&configuration)
	if query.Error != nil {
		return query.Error
	}

	aid := int(configuration.AID)
	env := configuration.Env
	zoneCode := configuration.Zone
	filename := configuration.FileName()

	// Get publish version
	var confHistory db.ConfigurationHistory
	query = mysql.Where("configuration_id=? and version =?", param.ID, param.Version).Find(&confHistory)
	if query.Error != nil {
		return query.Error
	}

	content := confHistory.Content
	version := confHistory.Version

	// resource filter
	content = configresource.FillConfigResource(content)
	// Get nodes data
	var instanceList []string
	if instanceList, err = getPublishInstance(aid, env, zoneCode); err != nil {
		return
	}

	// Obtain application management port
	appInfo, err := resource.Resource.GetApp(aid)
	if err != nil {
		return
	}

	// Save the configuration in etcd
	if err = publishETCD(view.ReqConfigPublish{
		AppName:      appInfo.AppName,
		ZoneCode:     zoneCode,
		Port:         appInfo.GovernPort,
		FileName:     filename,
		Format:       configuration.Format,
		Content:      content,
		InstanceList: instanceList,
		Env:          env,
		Version:      version,
	}); err != nil {
		return
	}

	tx := mysql.Begin()
	{
		// Publish record
		instanceListJSON, _ := json.Marshal(instanceList)

		var cp db.ConfigurationPublish
		cp.ApplyInstance = string(instanceListJSON)
		cp.ConfigurationID = configuration.ID
		cp.ConfigurationHistoryID = confHistory.ID
		cp.UID = uint(user.Uid)
		_, cp.FilePath = genConfigurePath(appInfo.AppName, configuration.FileName())
		if err = tx.Save(&cp).Error; err != nil {
			tx.Rollback()
			return
		}

		for _, instance := range instanceList {
			var cs db.ConfigurationStatus
			cs.ConfigurationID = configuration.ID
			cs.ConfigurationPublishID = cp.ID
			cs.HostName = instance
			cs.Used = 0
			cs.Synced = 0
			cs.TakeEffect = 0
			cs.CreatedAt = time.Now()
			cs.UpdateAt = time.Now()
			if err = tx.Save(&cs).Error; err != nil {
				tx.Rollback()
				return
			}
		}
		meta, _ := json.Marshal(cp)
		appevent.AppEvent.ConfgoFilePublishEvent(appInfo.Aid, appInfo.AppName, env, zoneCode, string(meta), user)

		tx.Commit()
	}

	return
}

func genConfigurePath(appName string, filename string) (pathArr []string, pathStr string) {
	for k, dir := range cfg.Cfg.Configure.Dirs {
		path := filepath.Join(dir, appName, "config", filename)
		pathArr = append(pathArr, path)
		if k == 0 {
			pathStr = path
		} else {
			pathStr = pathStr + ";" + path
		}
	}
	return
}

func getPublishInstance(aid int, env string, zoneCode string) (instanceList []string, err error) {
	nodes, _ := resource.Resource.GetAllAppNodeList(db.AppNode{
		Aid:      aid,
		Env:      env,
		ZoneCode: zoneCode,
	})
	if len(nodes) == 0 {
		return instanceList, fmt.Errorf(errorconst.ParamNoInstances.Code().String() + errorconst.ParamNoInstances.Name())
	}
	for _, node := range nodes {
		instanceList = append(instanceList, node.HostName)
	}
	return instanceList, nil
}

func configurationHeader(content, format, version string) string {
	if format == "toml" {
		headerVersion := fmt.Sprintf(`juno_configuration_version = "%s"`, version)
		content = fmt.Sprintf("%s\n%s", headerVersion, content)
	}
	return content
}

func publishETCD(req view.ReqConfigPublish) (err error) {

	content := configurationHeader(req.Content, req.Format, req.Version)

	xlog.Debug("func publishETCD", zap.String("content", content))

	paths, _ := genConfigurePath(req.AppName, req.FileName)
	data := view.ConfigurationPublishData{
		Content: content,
		Metadata: view.Metadata{
			Timestamp: time.Now().Unix(),
			Format:    req.Format,
			Version:   req.Version,
			Paths:     paths,
		},
	}
	var buf []byte
	if buf, err = json.Marshal(data); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()

	for _, hostName := range req.InstanceList {
		for _, prefix := range cfg.Cfg.Configure.Prefixes {
			key := fmt.Sprintf("/%s/%s/%s/%s/static/%s/%s", prefix, hostName, req.AppName, req.Env, req.FileName, req.Port)
			// The migration is complete, only write independent ETCD of the configuration center
			_, err = clientproxy.ClientProxy.EtcdPut(view.UniqZone{Env: req.Env, Zone: req.ZoneCode}, ctx, key, string(buf))
			if err != nil {
				return
			}

			// for k8s
			clusterKey := fmt.Sprintf("/%s/cluster/%s/%s/static/%s", prefix, req.AppName, req.Env, req.FileName)
			_, err = clientproxy.ClientProxy.EtcdPut(view.UniqZone{Env: req.Env, Zone: req.ZoneCode}, ctx, clusterKey, string(buf))
			if err != nil {
				return
			}

		}
	}
	return nil
}

// History 发布历史分页列表，Page从0开始
func History(param view.ReqHistoryConfig, uid int) (resp view.RespHistoryConfig, err error) {
	list := make([]db.ConfigurationHistory, 0)

	if param.Size == 0 {
		param.Size = 1
	}

	query := mysql.Where("configuration_id = ?", param.ID)

	wg := sync.WaitGroup{}
	errChan := make(chan error)
	doneChan := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()

		offset := param.Size * param.Page
		query := query.Preload("AccessToken").
			Preload("User").Limit(param.Size).Offset(offset).Order("id desc").Find(&list)
		if query.Error != nil {
			errChan <- query.Error
		}
	}()

	wg.Add(1)
	go func() {
		wg.Done()

		q := query.Model(&db.ConfigurationHistory{}).Count(&resp.Pagination.Total)
		if q.Error != nil {
			errChan <- q.Error
		}
	}()

	go func() {
		wg.Wait()

		doneChan <- struct{}{}
	}()

	select {
	case <-doneChan:
		break
	case e := <-errChan:
		close(errChan)
		err = e
		return
	}

	for _, item := range list {
		configItem := view.RespHistoryConfigItem{
			ID:              item.ID,
			UID:             item.UID,
			AccessTokenID:   item.AccessTokenID,
			ConfigurationID: item.ConfigurationID,
			Version:         item.Version,
			CreatedAt:       item.CreatedAt,
			ChangeLog:       item.ChangeLog,
		}

		configItem.UID = item.UID

		if item.AccessToken != nil {
			configItem.AccessTokenName = item.AccessToken.Name
		}

		if item.User != nil {
			configItem.UserName = item.User.Username
		}

		resp.List = append(resp.List, configItem)
	}

	resp.Pagination.Current = int(param.Page)
	resp.Pagination.PageSize = int(param.Size)

	return
}

// Diff ..
func Diff(configID, historyID uint) (resp view.RespDiffConfig, err error) {
	modifiedConfig := db.ConfigurationHistory{}
	err = mysql.Preload("Configuration").Preload("User").
		Where("id = ?", historyID).First(&modifiedConfig).Error
	if err != nil {
		return
	}

	originConfig := db.ConfigurationHistory{}
	err = mysql.Preload("Configuration").Preload("User").
		Where("id < ? and configuration_id = ?", historyID, configID).Order("id desc").First(&originConfig).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			resp.Origin = nil
			err = nil
		} else {
			return
		}
	} else {
		resp.Origin = &view.RespDetailConfig{
			ID:          originConfig.ID,
			AID:         originConfig.Configuration.AID,
			Name:        originConfig.Configuration.Name,
			Content:     originConfig.Content,
			Format:      originConfig.Configuration.Format,
			Env:         originConfig.Configuration.Env,
			Zone:        originConfig.Configuration.Env,
			CreatedAt:   originConfig.CreatedAt,
			UpdatedAt:   originConfig.Configuration.UpdatedAt,
			PublishedAt: originConfig.Configuration.PublishedAt,
		}
	}

	resp.Modified = view.RespDetailConfig{
		ID:          modifiedConfig.ID,
		AID:         modifiedConfig.Configuration.AID,
		Name:        modifiedConfig.Configuration.Name,
		Content:     modifiedConfig.Content,
		Format:      modifiedConfig.Configuration.Format,
		Env:         modifiedConfig.Configuration.Env,
		Zone:        modifiedConfig.Configuration.Env,
		CreatedAt:   modifiedConfig.CreatedAt,
		UpdatedAt:   modifiedConfig.Configuration.UpdatedAt,
		PublishedAt: modifiedConfig.Configuration.PublishedAt,
	}

	return
}

// Delete ..
func Delete(id uint) (err error) {
	err = mysql.Delete(&db.Configuration{}, "id = ?", id).Error
	return
}

func ReadInstanceConfig(param view.ReqReadInstanceConfig) (configContentList []view.RespReadInstanceConfigItem, err error) {
	var config db.Configuration
	var app db.AppInfo
	var node db.AppNode

	err = mysql.Where("id = ?", param.ConfigID).First(&config).Error
	if err != nil {
		return
	}

	err = mysql.Where("aid = ?", config.AID).First(&app).Error
	if err != nil {
		return
	}

	err = mysql.Where("app_name = ?", app.AppName).Where("host_name = ?", param.HostName).First(&node).Error
	if err != nil {
		return
	}

	zone := view.UniqZone{
		Env:  config.Env,
		Zone: config.Zone,
	}

	pathArr, _ := genConfigurePath(app.AppName, config.FileName())

	var eg errgroup.Group
	for _, configPath := range pathArr {
		eg.Go(func() error {
			var err error
			var plainText string
			var resp *resty.Response
			var configItem = view.RespReadInstanceConfigItem{
				ConfigID: config.ID,
				FileName: configPath,
			}
			var fileRead struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
				Data struct {
					Content string `json:"content"`
				} `json:"data"`
			}

			req := view.ReqHTTPProxy{
				Address: fmt.Sprintf("%s:%d", node.IP, cfg.Cfg.Agent.Port),
				URL:     "/api/agent/file",
				Type:    http.MethodGet,
				Params: map[string]string{
					"file_name": configPath,
				},
			}
			resp, err = clientproxy.ClientProxy.HttpGet(zone, req)
			if err != nil {
				goto End
			}

			err = json.Unmarshal(resp.Body(), &fileRead)
			if err != nil {
				goto End
			}

			if fileRead.Code != 200 {
				err = fmt.Errorf("%v", fileRead)
				goto End
			}

			plainText, err = agent.Decrypt(fileRead.Data.Content)
			if err != nil {
				err = errors.Wrap(err, "config file decrypt failed")
				goto End
			}

		End:
			if err != nil {
				configItem.Error = err.Error()
			}
			configItem.Content = plainText
			configContentList = append(configContentList, configItem)

			return nil
		})
	}

	// ignore error
	_ = eg.Wait()

	return
}

func GetAllConfigText() (list []db.Configuration, err error) {
	list = make([]db.Configuration, 0)
	err = mysql.Find(&list).Error
	return
}
