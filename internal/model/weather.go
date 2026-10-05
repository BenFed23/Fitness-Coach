package model

type IPLocation struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
	City      string  `json:"city"`
	Country   string  `json:"country"`
}

type CurrentWeather struct {
	Time               string  `json:"time"`
	Interval           int     `json:"interval"`
	Temperature2m      float64 `json:"temperature_2m"`
	RelativeHumidity2m float64 `json:"relative_humidity_2m"`
	Rain               float64 `json:"rain"`
	WindSpeed10m       float64 `json:"wind_speed_10m"`
}

type CurrentWeatherRaw struct {
	Time               string      `json:"time"`
	Interval           int         `json:"interval"`
	Temperature2m      float64     `json:"temperature_2m"`
	RelativeHumidity2m float64     `json:"relative_humidity_2m"`
	Rain               interface{} `json:"rain"`
	WindSpeed10m       float64     `json:"wind_speed_10m"`
}

type WeatherResponseRaw struct {
	Latitude  float64           `json:"latitude"`
	Longitude float64           `json:"longitude"`
	Timezone  string            `json:"timezone"`
	Current   CurrentWeatherRaw `json:"current"`
}

type WeatherResponse struct {
	Latitude  float64        `json:"latitude"`
	Longitude float64        `json:"longitude"`
	Timezone  string         `json:"timezone"`
	Current   CurrentWeather `json:"current"`
}
