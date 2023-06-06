package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	promclient "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	yaml "gopkg.in/yaml.v2"
	"io/ioutil"
	"lib.hemtjan.st/client"
	"lib.hemtjan.st/device"
	mqtt "lib.hemtjan.st/transport/mqtt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

var (
	flgConfig = flag.String("config", "./config.yaml", "Config file name")
	dbg       = log.New(ioutil.Discard, "[DEBUG] ", log.LstdFlags)
)

type Decoder interface {
	Decode(interface{}) error
}

type MetricMap struct {
	Topic    string `json:"topic,omitempty" yaml:"topic,omitempty"`
	topicTpl *template.Template
}

type ScrapeCfg struct {
	Device   *device.Info          `json:"device,omitempty" yaml:"device,omitempty"`
	URL      string                `json:"url,omitempty" yaml:"url,omitempty"`
	Interval string                `json:"interval,omitempty" yaml:"interval,omitempty"`
	Match    []string              `json:"match,omitempty" yaml:"match,omitempty"`
	Topic    string                `json:"topic,omitempty" yaml:"topic,omitempty"`
	Retain   bool                  `json:"retain,omitempty" yaml:"retain,omitempty"`
	Map      map[string]*MetricMap `json:"map,omitempty" yaml:"map,omitempty"`

	interval time.Duration
	match    []*regexp.Regexp
	topicTpl *template.Template
}

func (s *ScrapeCfg) TopicFor(m *Metric) (string, error) {
	if s.Map != nil {
		for k, v := range s.Map {
			if k == m.Name && v.topicTpl != nil {
				wr := &strings.Builder{}
				err := v.topicTpl.Execute(wr, m)
				return wr.String(), err
			}
		}
	}
	if s.topicTpl != nil {
		wr := &strings.Builder{}
		err := s.topicTpl.Execute(wr, m)
		return wr.String(), err
	}
	return "prometheus/" + m.Name, nil
}

type Metric struct {
	Name  string
	Label map[string]string
	promclient.Metric
}

type Config struct {
	Scrape []*ScrapeCfg `json:"scrape,omitempty" yaml:"scrape,omitempty"`
}

func loadCfg() (*Config, error) {
	extOff := strings.LastIndex(*flgConfig, ".")
	if extOff < 0 {
		extOff = 0
	}
	ext := strings.ToLower((*flgConfig)[extOff:])
	var dec Decoder

	f, err := os.Open(*flgConfig)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", *flgConfig, err)
	}
	switch ext {
	case ".json":
		dec = json.NewDecoder(f)
	case ".yaml", ".yml":
		dec = yaml.NewDecoder(f)
	default:
		return nil, fmt.Errorf("unsupported config extension: %s", ext)
	}
	cfg := &Config{}
	if err := dec.Decode(cfg); err != nil {
		return nil, err
	}

	for _, s := range cfg.Scrape {
		for _, m := range s.Match {
			mr, err := regexp.Compile(m)
			if err != nil {
				return nil, fmt.Errorf("in match '%s': %v", m, err)
			}
			s.match = append(s.match, mr)
		}
		s.interval = 10 * time.Second
		if s.Interval != "" {
			if dur, err := time.ParseDuration(s.Interval); err != nil {
				return nil, fmt.Errorf("parsing interval '%s': %w", s.Interval, err)
			} else if dur >= 50*time.Millisecond {
				s.interval = dur
			}
		}
		if up, err := url.Parse(s.URL); err != nil || s.URL == "" {
			return nil, fmt.Errorf("invalid url '%s': %w", s.URL, err)
		} else if up.Scheme != "http" && up.Scheme != "https" {
			return nil, fmt.Errorf("invalid url '%s': %v", s.URL, "only http and https scheme supported")
		}

		if s.Topic != "" {
			s.topicTpl, err = template.New("topic").Funcs(helpers()).Parse(s.Topic)
			if err != nil {
				return nil, fmt.Errorf("error parsing topic '%s': %w", s.Topic, err)
			}
		}

		if s.Map != nil {
			for _, m := range s.Map {
				if m == nil {
					continue
				}
				if m.Topic != "" {
					m.topicTpl, err = template.New("topic").Funcs(helpers()).Parse(m.Topic)
					if err != nil {
						return nil, fmt.Errorf("error parsing topic '%s': %w", m.Topic, err)
					}
				}
			}
		}
	}

	return cfg, nil
}

func start(ctx context.Context, cfg *Config, mq mqtt.MQTT) error {
	if len(cfg.Scrape) == 0 {
		return fmt.Errorf("no scrapers configured")
	}
	wg := sync.WaitGroup{}
	for _, s := range cfg.Scrape {
		wg.Add(1)
		go func(s *ScrapeCfg) {
			defer wg.Done()
			startScrape(ctx, s, mq)
		}(s)
		if s.Device != nil {
			_, err := client.NewDevice(s.Device, mq)
			if err != nil {
				log.Printf("Unable to create device (%+v): %s", s.Device, err)
			}
		}
	}
	wg.Wait()
	return nil
}

func startScrape(ctx context.Context, cfg *ScrapeCfg, mq mqtt.MQTT) {
	dur, err := time.ParseDuration(cfg.Interval)
	if dur < 1*time.Millisecond || err != nil {
		log.Printf("Invalid interval '%s': %v - defaulting to 10s", cfg.Interval, err)
		dur = 10 * time.Second
	}

	ticker := time.NewTicker(dur)

	out := map[string]string{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		outNew := map[string]string{}

		resp, err := http.Get(cfg.URL)
		if err != nil {
			log.Printf("Fetching %s: %v", cfg.URL, err)
			continue
		}

		var parser expfmt.TextParser
		mf, err := parser.TextToMetricFamilies(resp.Body)
		if err != nil {
			log.Printf("Parsing %s: %v", cfg.URL, err)
			continue
		}

		for k, v := range mf {
			if len(cfg.match) > 0 {
				found := false
				for _, mm := range cfg.match {
					if found = mm.Match([]byte(v.GetName())); found {
						break
					}
				}
				if !found {
					continue
				}
			}

			for _, m := range v.GetMetric() {
				met := &Metric{
					Name:   k,
					Metric: *m,
					Label:  map[string]string{},
				}
				for _, lbl := range m.GetLabel() {
					met.Label[lbl.GetName()] = lbl.GetValue()
				}

				topic, err := cfg.TopicFor(met)
				if err != nil {
					log.Printf("Error getting topic for %s: %v", k, err)
				}
				var val float64
				if g := met.GetGauge(); g != nil {
					val = g.GetValue()
				} else if g := met.GetCounter(); g != nil {
					val = g.GetValue()
				} else if g := met.GetUntyped(); g != nil {
					val = g.GetValue()
					/*} else if g := met.GetHistogram(); g != nil {
					if g.GetSampleCount() > 0 {
						val = g.GetSampleSum() / float64(g.GetSampleCount())
					} else {
						val = 0
					}
					*/
				} else {
					log.Printf("[%s] Unsupported metric type: %+v", k, m)
					continue
				}

				valStr := fmt.Sprintf("%f", val)

				if cur, ok := outNew[topic]; ok && len(cur) > 0 {
					valStr = cur + "," + valStr
				}
				outNew[topic] = valStr
			}
		}
		for k, v := range outNew {
			if k == "" || v == "" {
				continue
			}
			if vv, ok := out[k]; ok && vv == v {
				dbg.Printf("Skipping %s (no update)", k)
				continue
			}
			dbg.Printf("Sending to %s: '%s'", k, v)
			mq.Publish(k, []byte(v), cfg.Retain)
		}

		out = outNew
	}
}

func main() {
	mqFlags := mqtt.Flags(flag.String, flag.Bool)
	flgDbg := flag.Bool("debug", false, "Enable verbose logging")
	flag.Parse()

	if *flgDbg {
		dbg.SetOutput(os.Stderr)
	}

	ctx, stop := context.WithCancel(context.Background())

	cfg, err := loadCfg()
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	reloadCh := make(chan os.Signal, 1)

	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	signal.Notify(reloadCh, syscall.SIGHUP)

	go func() {
		forceQuit := false
		for {
			sig := <-sigCh
			if forceQuit {
				log.Fatalf("Got signal %v, terminating forcefully", sig)
			} else {
				forceQuit = true
				log.Printf("Got signal %v, terminating gracefully", sig)
				stop()
			}
		}
	}()
	wg := sync.WaitGroup{}

	mqCfg, err := mqFlags()
	if err != nil {
		log.Fatalf("Invalid MQTT configuration: %v", err)
	}
	mq, err := mqtt.New(ctx, mqCfg)
	if err != nil {
		log.Fatalf("Invalid MQTT configuration: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer stop()
		for {
			ok, err := mq.Start()
			if err != nil {
				log.Printf("MQTT Error: %v", err)
			}
			if !ok {
				// Non-recoverable error
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()

	for {
		subCtx, cancel := context.WithCancel(ctx)
		wg.Add(1)
		log.Printf("Starting scraper")
		go func() {
			defer wg.Done()
			if err := start(subCtx, cfg, mq); err != nil {
				log.Printf("Error starting scraper: %v", err)
			} else {
				log.Printf("Scraper stopped")
			}
		}()

		for {
			select {
			case <-reloadCh:
				newCfg, err := loadCfg()
				if err != nil {
					log.Printf("Error loading config: %v", err)
					continue
				}
				log.Printf("Reloading")
				cfg = newCfg
				cancel()
				wg.Wait()
				break
			case <-ctx.Done():
				cancel()
				wg.Wait()
				return
			}
		}
	}

}

func helpers() template.FuncMap {
	return template.FuncMap{
		"ToUpper": strings.ToUpper,
		"ToLower": strings.ToLower,
		"Join":    strings.Join,
		"Split":   strings.Split,
		"Sprintf": fmt.Sprintf,
		"ToJSON": func(v interface{}) string {
			b, _ := json.Marshal(v)
			return string(b)
		},
	}
}
