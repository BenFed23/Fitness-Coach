package weather

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"fitness-agent/internal/model"
)

const (
	weatherAPIBase   = "https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&current=temperature_2m,relative_humidity_2m,rain,wind_speed_10m"
	ipLocationAPIURL = "http://ip-api.com/json/"
	httpTimeout      = 10 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

func GetCurrentLocation(ctx context.Context) (*model.IPLocation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipLocationAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build location request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch IP location: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("location service returned status %d: %s", resp.StatusCode, string(body))
	}

	var loc model.IPLocation
	if err := json.NewDecoder(resp.Body).Decode(&loc); err != nil {
		return nil, fmt.Errorf("decode location response: %w", err)
	}
	return &loc, nil
}

func FetchWeatherData(ctx context.Context, lat, lon float64) (*model.WeatherResponse, error) {
	apiURL := fmt.Sprintf(weatherAPIBase, lat, lon)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build weather request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch weather data: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("weather service returned status %d: %s", resp.StatusCode, string(body))
	}

	var raw model.WeatherResponseRaw
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode weather response: %w", err)
	}

	rainVal := 0.0
	switch v := raw.Current.Rain.(type) {
	case float64:
		rainVal = v
	case string:
		if parsed, parseErr := strconv.ParseFloat(v, 64); parseErr == nil {
			rainVal = parsed
		}
	}

	return &model.WeatherResponse{
		Latitude:  raw.Latitude,
		Longitude: raw.Longitude,
		Timezone:  raw.Timezone,
		Current: model.CurrentWeather{
			Time:               raw.Current.Time,
			Interval:           raw.Current.Interval,
			Temperature2m:      raw.Current.Temperature2m,
			RelativeHumidity2m: raw.Current.RelativeHumidity2m,
			Rain:               rainVal,
			WindSpeed10m:       raw.Current.WindSpeed10m,
		},
	}, nil
}
