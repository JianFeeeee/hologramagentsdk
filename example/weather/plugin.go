package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

type Plugin struct {
	name       string
	sdk        *sdk.PluginSDK
	client     *http.Client
	defaultLoc string
}

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &Plugin{name: name}, nil
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)
	p.sdk = s
	p.client = &http.Client{Timeout: 15 * time.Second}

	s.Settings().RegisterDef(sdk.ConfigDef{
		Key: "default_location", Default: "", Type: "string",
		DisplayName: "Default Location", Description: "Default city name for weather queries, e.g. Beijing",
		Category: "weather",
	})

	if v, _ := s.Settings().Get("default_location"); v != nil {
		if vs, ok := v.(string); ok {
			p.defaultLoc = vs
		}
	}

	tp := p.name + "_"
	s.RegisterTool(tp+"current", sdk.ToolDef{
		Name: tp + "current", Description: "Get current weather for a city",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location": map[string]interface{}{"type": "string", "description": "City name (e.g. Beijing, Shanghai, London). Uses default if omitted."},
				"units":    map[string]interface{}{"type": "string", "description": "Units: metric (celsius) or imperial (fahrenheit), default metric"},
			},
		},
		// NoMemory: 外部实时数据对记忆计算无长期价值，跳过向量化/关键词提取
		NoMemory: true,
		// Cleaner: 工具输出参与记忆计算前先过滤；这里演示用法（保留摘要行）
		Cleaner: func(output string) string {
			for _, line := range strings.Split(output, "\n") {
				if strings.HasPrefix(line, "🌤") {
					return line
				}
			}
			return output
		},
	}, p.handleCurrent)

	s.RegisterTool(tp+"forecast", sdk.ToolDef{
		Name: tp + "forecast", Description: "Get weather forecast for next several days",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location": map[string]interface{}{"type": "string", "description": "City name. Uses default if omitted."},
				"days":     map[string]interface{}{"type": "integer", "description": "Number of days (1-7), default 3"},
				"units":    map[string]interface{}{"type": "string", "description": "Units: metric or imperial, default metric"},
			},
		},
		NoMemory: true,
	}, p.handleForecast)

	s.RegisterTool(tp+"set_location", sdk.ToolDef{
		Name: tp + "set_location", Description: "Set default weather location",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location": map[string]interface{}{"type": "string", "description": "City name to set as default"},
			},
			"required": []string{"location"},
		},
		NoMemory: true,
	}, p.handleSetLocation)

	// 阶段钩子：own_tools 作用域——仅在本插件的工具被调用时触发
	s.RegisterStage(sdk.StageAfterToolcall, func(ctx *sdk.StageContext) error {
		ctx.Lock()
		defer ctx.Unlock()
		if len(ctx.ToolResults) > 0 {
			log.Printf("[%s] stage after_toolcall(own): %s", p.name, ctx.ToolResults[0].Name)
		}
		return nil
	}, sdk.StageScopeOwnTools)

	// 输出通道：把天气结果主动推给用户（如 QQ/WebUI 渠道）
	if err := s.RegisterOutputChannel(tp+"weather_out", 0, "push weather to user", sdk.ChannelDef{
		NoMemory: true,
	}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		return map[string]interface{}{"content": "weather pushed: " + payload}, nil
	}); err != nil {
		return err
	}

	// 输入通道：接收天气订阅请求（NoMemory: 通道输入不参与记忆计算）
	if err := s.RegisterInputChannel(tp+"weather_in", sdk.ChannelDef{NoMemory: true}); err != nil {
		return err
	}

	log.Printf("[%s] started", p.name)
	return nil
}

func (p *Plugin) Stop() error {
	log.Printf("[%s] stopped", p.name)
	return nil
}

type wttrResp struct {
	CurrentCondition []struct {
		TempC          string `json:"temp_C"`
		FeelsLikeC     string `json:"FeelsLikeC"`
		Humidity       string `json:"humidity"`
		WindspeedKmph  string `json:"windspeedKmph"`
		Winddir16Point string `json:"winddir16Point"`
		Pressure       string `json:"pressure"`
		Visibility     string `json:"visibility"`
		WeatherDesc    []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
		LocalObsDateTime string `json:"localObsDateTime"`
	} `json:"current_condition"`
	NearestArea []struct {
		AreaName []struct {
			Value string `json:"value"`
		} `json:"areaName"`
		Country []struct {
			Value string `json:"value"`
		} `json:"country"`
		Region []struct {
			Value string `json:"value"`
		} `json:"region"`
	} `json:"nearest_area"`
	Weather []wttrDay `json:"weather"`
}

type wttrDay struct {
	Date      string `json:"date"`
	Astronomy []struct {
		Sunrise string `json:"sunrise"`
		Sunset  string `json:"sunset"`
	} `json:"astronomy"`
	MaxtempC string `json:"maxtempC"`
	MintempC string `json:"mintempC"`
	Hourly   []struct {
		TempC       string `json:"tempC"`
		WeatherDesc []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
		WindspeedKmph  string `json:"windspeedKmph"`
		Winddir16Point string `json:"winddir16Point"`
		Humidity       string `json:"humidity"`
		FeelsLikeC     string `json:"FeelsLikeC"`
		PrecipMM       string `json:"precipMM"`
		Visibility     string `json:"visibility"`
	} `json:"hourly"`
}

func (p *Plugin) getLoc(args map[string]interface{}) string {
	if v, ok := args["location"].(string); ok && v != "" {
		return v
	}
	return p.defaultLoc
}

func (p *Plugin) getUnits(args map[string]interface{}) string {
	if v, ok := args["units"].(string); ok && (v == "imperial" || v == "metric") {
		return v
	}
	return "metric"
}

func (p *Plugin) fetchWttr(location string) (*wttrResp, error) {
	url := fmt.Sprintf("https://wttr.in/%s?format=j1", strings.ReplaceAll(location, " ", "%20"))
	resp, err := p.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var data wttrResp
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if len(data.CurrentCondition) == 0 {
		return nil, fmt.Errorf("no weather data for: %s", location)
	}
	return &data, nil
}

func (p *Plugin) displayName(data *wttrResp) string {
	if len(data.NearestArea) == 0 {
		return "Unknown"
	}
	area := data.NearestArea[0]
	name := ""
	if len(area.AreaName) > 0 {
		name = area.AreaName[0].Value
	}
	region := ""
	if len(area.Region) > 0 {
		region = area.Region[0].Value
	}
	country := ""
	if len(area.Country) > 0 {
		country = area.Country[0].Value
	}
	var parts []string
	if name != "" {
		parts = append(parts, name)
	}
	if region != "" && region != name {
		parts = append(parts, region)
	}
	if country != "" {
		parts = append(parts, country)
	}
	return strings.Join(parts, ", ")
}

func convertCtoF(c string) string {
	if v, err := strconv.ParseFloat(c, 64); err == nil {
		return fmt.Sprintf("%.0f", v*9/5+32)
	}
	return c
}

func (p *Plugin) handleCurrent(args map[string]interface{}) (interface{}, error) {
	location := p.getLoc(args)
	if location == "" {
		return map[string]interface{}{"isError": true, "content": "No location specified. Provide a city name or set default_location."}, nil
	}

	units := p.getUnits(args)

	data, err := p.fetchWttr(location)
	if err != nil {
		return map[string]interface{}{"isError": true, "content": "Weather request failed: " + err.Error()}, nil
	}

	cc := data.CurrentCondition[0]
	place := p.displayName(data)

	desc := ""
	if len(cc.WeatherDesc) > 0 {
		desc = cc.WeatherDesc[0].Value
	}

	unitStr := "°C"
	windUnit := "km/h"
	tempStr := cc.TempC
	feelsStr := cc.FeelsLikeC
	if units == "imperial" {
		unitStr = "°F"
		windUnit = "mph"
		tempStr = convertCtoF(tempStr)
		feelsStr = convertCtoF(feelsStr)
	}

	obsTime := cc.LocalObsDateTime
	if len(obsTime) > 16 {
		obsTime = obsTime[:16]
	}

	result := fmt.Sprintf("🌤 %s — %s\n🌡 %s%s (体感 %s%s)\n💧 湿度 %s%%  |  💨 风速 %s %s %s\n🕐 %s",
		place, desc,
		tempStr, unitStr, feelsStr, unitStr,
		cc.Humidity, cc.WindspeedKmph, windUnit, cc.Winddir16Point,
		obsTime)

	return map[string]interface{}{
		"content":    result,
		"location":   place,
		"temp":       cc.TempC,
		"feels_like": cc.FeelsLikeC,
		"humidity":   cc.Humidity,
		"wind_speed": cc.WindspeedKmph,
		"weather":    desc,
		"observed":   obsTime,
	}, nil
}

func (p *Plugin) handleForecast(args map[string]interface{}) (interface{}, error) {
	location := p.getLoc(args)
	if location == "" {
		return map[string]interface{}{"isError": true, "content": "No location specified."}, nil
	}

	days := 3
	if v, ok := args["days"].(float64); ok {
		d := int(v)
		if d >= 1 && d <= 7 {
			days = d
		}
	}

	units := p.getUnits(args)

	data, err := p.fetchWttr(location)
	if err != nil {
		return map[string]interface{}{"isError": true, "content": "Forecast request failed: " + err.Error()}, nil
	}

	place := p.displayName(data)

	unitStr := "°C"
	if units == "imperial" {
		unitStr = "°F"
	}

	dayCount := days
	if dayCount > len(data.Weather) {
		dayCount = len(data.Weather)
	}
	daysData := data.Weather[:dayCount]

	var lines []string
	lines = append(lines, fmt.Sprintf("📅 %d日天气预报 — %s", days, place))
	for _, day := range daysData {
		t, err := time.Parse("2006-01-02", day.Date)
		if err != nil {
			continue
		}
		weekday := t.Weekday().String()[:3]

		maxT := day.MaxtempC
		minT := day.MintempC
		desc := ""
		precip := ""

		if len(day.Hourly) > 0 {
			mid := len(day.Hourly) / 2
			if len(day.Hourly[mid].WeatherDesc) > 0 {
				desc = day.Hourly[mid].WeatherDesc[0].Value
			}
			totalPrecip := 0.0
			for _, h := range day.Hourly {
				if pv, err := strconv.ParseFloat(h.PrecipMM, 64); err == nil {
					totalPrecip += pv
				}
			}
			if totalPrecip > 0 {
				precip = fmt.Sprintf(" 🌧%.1fmm", totalPrecip)
			}
		}

		if units == "imperial" {
			maxT = convertCtoF(maxT)
			minT = convertCtoF(minT)
		}

		sunrise, sunset := "", ""
		if len(day.Astronomy) > 0 {
			sunrise = day.Astronomy[0].Sunrise
			sunset = day.Astronomy[0].Sunset
		}

		datePart := ""
		if len(day.Date) >= 8 {
			datePart = day.Date[5:7] + "/" + day.Date[8:]
		}
		line := fmt.Sprintf("  %s %s — %s~%s%s %s", weekday, datePart, minT, maxT, unitStr, desc)
		if precip != "" {
			line += precip
		}
		if sunrise != "" && sunset != "" {
			line += fmt.Sprintf(" 🌅%s 🌇%s", sunrise, sunset)
		}
		lines = append(lines, line)
	}

	cc := data.CurrentCondition[0]
	nowDesc := ""
	if len(cc.WeatherDesc) > 0 {
		nowDesc = cc.WeatherDesc[0].Value
	}
	lines = append(lines, fmt.Sprintf("\n当前：%s %s°C", nowDesc, cc.TempC))

	return map[string]interface{}{
		"content":  strings.Join(lines, "\n"),
		"location": place,
	}, nil
}

func (p *Plugin) handleSetLocation(args map[string]interface{}) (interface{}, error) {
	loc, _ := args["location"].(string)
	if loc == "" {
		return map[string]interface{}{"isError": true, "content": "Location is required"}, nil
	}

	p.sdk.Settings().Set("default_location", loc)
	p.defaultLoc = loc
	return map[string]interface{}{"content": fmt.Sprintf("Default location set to: %s", loc)}, nil
}
