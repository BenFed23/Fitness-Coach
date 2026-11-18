package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	weatherAPIBase   = "https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&current=temperature_2m,relative_humidity_2m,rain,wind_speed_10m"
	ipLocationAPIURL = "http://ip-api.com/json/"
)

// IPLocation is the approximate location inferred from the public IP address.
type IPLocation struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
}

// CurrentWeather contains Open-Meteo's current weather measurements.
type CurrentWeather struct {
	Time               string  `json:"time"`
	Interval           int     `json:"interval"`
	Temperature2m      float64 `json:"temperature_2m"`
	RelativeHumidity2m float64 `json:"relative_humidity_2m"`
	Rain               float64 `json:"rain"`
	WindSpeed10m       float64 `json:"wind_speed_10m"`
}

type CurrentUnits struct {
	Temperature2m      string `json:"temperature_2m"`
	RelativeHumidity2m string `json:"relative_humidity_2m"`
	Rain               string `json:"rain"`
	WindSpeed10m       string `json:"wind_speed_10m"`
}

type WeatherResponse struct {
	Latitude     float64        `json:"latitude"`
	Longitude    float64        `json:"longitude"`
	Timezone     string         `json:"timezone"`
	Current      CurrentWeather `json:"current"`
	CurrentUnits CurrentUnits   `json:"current_units"`
}

// GetCurrentLocation returns AGENT_LATITUDE/AGENT_LONGITUDE (and optional
// AGENT_CITY) when set, otherwise the approximate location of the public IP.
// On a cloud server the IP points at the data center, so set them there.
func GetCurrentLocation() (*IPLocation, error) {
	if lat, lon := os.Getenv("AGENT_LATITUDE"), os.Getenv("AGENT_LONGITUDE"); lat != "" || lon != "" {
		var loc IPLocation
		if _, err := fmt.Sscan(lat, &loc.Latitude); err != nil {
			return nil, fmt.Errorf("invalid AGENT_LATITUDE %q: %w", lat, err)
		}
		if _, err := fmt.Sscan(lon, &loc.Longitude); err != nil {
			return nil, fmt.Errorf("invalid AGENT_LONGITUDE %q: %w", lon, err)
		}
		loc.City = firstNonEmpty(os.Getenv("AGENT_CITY"), "configured location")
		return &loc, nil
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(ipLocationAPIURL)
	if err != nil {
		return nil, fmt.Errorf("fetch IP location: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("location service returned status %d: %s", resp.StatusCode, body)
	}

	var loc IPLocation
	if err := json.NewDecoder(resp.Body).Decode(&loc); err != nil {
		return nil, fmt.Errorf("decode location response: %w", err)
	}
	return &loc, nil
}

func FetchWeatherData(url string) (*WeatherResponse, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch weather data: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("weather service returned status %d: %s", resp.StatusCode, body)
	}

	var weather WeatherResponse
	if err := json.NewDecoder(resp.Body).Decode(&weather); err != nil {
		return nil, fmt.Errorf("decode weather response: %w", err)
	}
	return &weather, nil
}

// evaluateWeather scores each weather reading independently.
func evaluateWeather(w *WeatherResponse) []Factor {
	c := w.Current
	factors := []Factor{
		evaluateTemperature(c.Temperature2m),
		evaluateHumidity(c.RelativeHumidity2m),
		evaluateRain(c.Rain),
		evaluateWind(c.WindSpeed10m),
	}
	return factors
}

func evaluateTemperature(tempC float64) Factor {
	f := Factor{Name: "Temperature", Value: tempC, Unit: "°C"}
	switch {
	case tempC >= 35 || tempC <= 0:
		f.Severity = Hazardous
		f.Note = "Extreme temperature; heat stroke or cold-injury risk."
	case tempC >= 30 || tempC <= 5:
		f.Severity = Caution
		f.Note = "Uncomfortable temperature; pace yourself and hydrate."
	default:
		f.Severity = Good
		f.Note = "Comfortable range for outdoor training."
	}
	return f
}

func evaluateHumidity(humidityPct float64) Factor {
	f := Factor{Name: "Humidity", Value: humidityPct, Unit: "%"}
	switch {
	case humidityPct >= 90:
		f.Severity = Hazardous
		f.Note = "Very high humidity severely limits sweat evaporation."
	case humidityPct >= 75:
		f.Severity = Caution
		f.Note = "High humidity; cooling is less effective than usual."
	default:
		f.Severity = Good
		f.Note = "Humidity within a manageable range."
	}
	return f
}

func evaluateRain(rainMM float64) Factor {
	f := Factor{Name: "Rain", Value: rainMM, Unit: "mm"}
	switch {
	case rainMM >= 10:
		f.Severity = Hazardous
		f.Note = "Heavy rain; poor visibility and slick surfaces."
	case rainMM > 0:
		f.Severity = Caution
		f.Note = "Light rain; wear appropriate gear."
	default:
		f.Severity = Good
		f.Note = "No rain expected."
	}
	return f
}

func evaluateWind(windKMH float64) Factor {
	f := Factor{Name: "Wind", Value: windKMH, Unit: "km/h"}
	switch {
	case windKMH >= 40:
		f.Severity = Hazardous
		f.Note = "Strong wind; risk of being blown off balance, falling debris."
	case windKMH >= 25:
		f.Severity = Caution
		f.Note = "Noticeable wind; expect a slower pace outdoors."
	default:
		f.Severity = Good
		f.Note = "Calm conditions."
	}
	return f
}
