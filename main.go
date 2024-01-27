package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Source struct {
	Domain   string
	Username string
	Token    string
	Exclude  []string
	Include  []string
}

func (s *Source) String() string {
	if s.Username != "" {
		return fmt.Sprintf("%s@%s", s.Username, s.Domain)
	}
	return s.Domain
}

type Config struct {
	Sources     []*Source
	Destination string
}

type Stat struct {
	Source       *Source
	Repos        []*Repo
	Skipped      int
	Mirrored     int
	Updated      int
	Failed       int
	FailedMirror int
	FailedUpdate int
}

func main() {
	config, err := loadConfig()
	if err != nil {
		log.Fatal("Failed to load config: ", err)
	}

	err = os.MkdirAll(config.Destination, 0755)
	if err != nil {
		if !os.IsExist(err) {
			log.Fatal("Failed to create destination directory: ", err)
		}
	}

	var stats []*Stat
	for _, source := range config.Sources {
		stat := &Stat{
			Source: source,
		}
		stats = append(stats, stat)
		repos, err := getRepo(source)
		if err != nil {
			log.Printf("Failed to get source [%s] repos. error:'%s'", source, err)
			continue
		}
		stat.Repos = repos
		log.Printf("Found %d repos for source [%s]", len(repos), source)
		for _, repo := range repos {
			remote := repo.HTTPURLToRepo
			local := fmt.Sprintf("%s.git", filepath.Join(config.Destination, source.Domain, repo.PathWithNamespace))
			if skip(source, remote) {
				stat.Skipped++
				continue
			}
			_, err := os.Stat(local)
			if err != nil {
				if !os.IsNotExist(err) {
					log.Printf("Failed to stat [%s]: %s", local, err)
					stat.Failed++
					continue
				}
				url := remote
				log.Printf("Mirroring [%s] -> [%s]", remote, local)
				_, err := clone(url, local)
				if err != nil {
					log.Printf("Failed mirror [%s] -> [%s]: clone error:'%s'", remote, local, err)
					remove(local)
					stat.FailedMirror++
					continue
				}
				_, err = disablegc(local)
				if err != nil {
					log.Printf("Failed mirror [%s] -> [%s]: disablegc error:'%s'", remote, local, err)
					remove(local)
					stat.FailedMirror++
					continue
				}
				_, err = touch(local)
				if err != nil {
					log.Printf("Failed mirror [%s] -> [%s]: touch error:'%s'", remote, local, err)
					remove(local)
					stat.FailedMirror++
					continue
				}
				largestsize, _, err := objects(local)
				if err != nil {
					log.Printf("Failed mirror [%s] -> [%s]: objects error:'%s'", remote, local, err)
					remove(local)
					stat.FailedMirror++
					continue
				}
				if largestsize > 95*1024*1024 {
					log.Printf("Should repack [%s]. objects largestsize=%d", local, largestsize)
					_, err = repack(local)
					if err != nil {
						log.Printf("Failed mirror [%s] -> [%s]: repack error:'%s'", remote, local, err)
						remove(local)
						stat.FailedMirror++
						continue
					}
					log.Printf("Repack [%s] finished.", local)
				}
				_, err = update(local)
				if err != nil {
					log.Printf("Failed mirror [%s] -> [%s]. update error:'%s'", remote, local, err)
					remove(local)
					stat.FailedMirror++
					continue
				}
				log.Printf("Successfully mirror [%s] -> [%s]", remote, local)
				stat.Mirrored++
			} else {
				log.Printf("Updating [%s] -> [%s]", remote, local)
				_, err = disablegc(local)
				if err != nil {
					log.Printf("Failed update [%s] -> [%s]: disablegc error:'%s'", remote, local, err)
					stat.FailedUpdate++
					continue
				}
				_, err := update(local)
				if err != nil {
					log.Printf("Failed update [%s] -> [%s] error: %s", remote, local, err)
					stat.FailedUpdate++
					continue
				}
				log.Printf("Successfully update [%s] -> [%s]", remote, local)
				stat.Updated++
			}
		}
	}
	for _, stat := range stats {
		log.Printf("Source [%s] stats: repos:%d skipped:%d mirrored:%d updated:%d failed:%d failed_mirror:%d failed_update:%d", stat.Source, len(stat.Repos), stat.Skipped, stat.Mirrored, stat.Updated, stat.Failed, stat.FailedMirror, stat.FailedUpdate)
	}
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile("config.json")
	if err != nil {
		return nil, err
	}
	config := &Config{}
	err = json.Unmarshal(b, config)
	if err != nil {
		return nil, err
	}
	return config, nil
}

type Repo struct {
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	NameWithNamespace string    `json:"name_with_namespace"`
	Path              string    `json:"path"`
	PathWithNamespace string    `json:"path_with_namespace"`
	CreatedAt         time.Time `json:"created_at"`
	HTTPURLToRepo     string    `json:"http_url_to_repo"`
}

func getRepo(source *Source) ([]*Repo, error) {
	var repos []*Repo
	page := 1
	perPage := 50
	for {
		pageRepos, err := getRepoPage(source, page, perPage)
		if err != nil {
			return nil, err
		}
		if len(pageRepos) == 0 {
			break
		}
		repos = append(repos, pageRepos...)
		page++
	}
	return repos, nil
}

func getRepoPage(source *Source, page, perPage int) ([]*Repo, error) {
	url := fmt.Sprintf("https://%s/api/v4/projects?simple=true&page=%d&per_page=%d&order_by=id&sort=asc", source.Domain, page, perPage)
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if source.Token != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", source.Token))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var repos []*Repo
	err = json.NewDecoder(resp.Body).Decode(&repos)
	if err != nil {
		return nil, err
	}
	return repos, nil
}

func matches(s []string, e string) bool {
	for _, v := range s {
		if v == e {
			return true
		}
		matched, err := filepath.Match(v, e)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func skip(source *Source, remote string) bool {
	if matches(source.Exclude, remote) {
		return true
	}
	if len(source.Include) > 0 && !matches(source.Include, remote) {
		return true
	}
	return false
}

func clone(url, local string) (*exec.Cmd, error) {
	cmd := exec.Command("git", "clone", "--mirror", url, local)
	err := cmd.Run()
	return cmd, err
}

func touch(local string) (*exec.Cmd, error) {
	cmd := exec.Command("touch", filepath.Join(local, "refs", ".gitkeep"), filepath.Join(local, "objects", ".gitkeep"))
	err := cmd.Run()
	return cmd, err
}

func objects(local string) (largestsize int64, count int64, err error) {
	err = filepath.WalkDir(filepath.Join(local, "objects"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		count++
		if !strings.HasSuffix(d.Name(), ".pack") {
			return nil
		}
		fi, _err := d.Info()
		if _err != nil {
			return _err
		}
		if fi.Size() >= largestsize {
			largestsize = fi.Size()
		}
		return nil
	})
	return
}
func repack(local string) (*exec.Cmd, error) {
	cmd := exec.Command("git", "-C", local, "repack", "--max-pack-size=95m", "-A", "-d")
	err := cmd.Run()
	return cmd, err
}

func update(local string) (*exec.Cmd, error) {
	cmd := exec.Command("git", "-C", local, "remote", "update")
	err := cmd.Run()
	return cmd, err
}

func disablegc(local string) (*exec.Cmd, error) {
	cmd := exec.Command("git", "-C", local, "config", "--local", "gc.auto", "0")
	err := cmd.Run()
	return cmd, err
}

func remove(local string) (*exec.Cmd, error) {
	cmd := exec.Command("rm", "-rf", local)
	err := cmd.Run()
	return cmd, err
}
