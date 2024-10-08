package web

import (
	"compress/gzip"
	"context"
	"errors"
	"strings"

	"github.com/goccy/go-json"

	"fmt"
	"io"
	"net/http"
	"runtime"

	"time"

	"github.com/amir20/dozzle/internal/docker"
	"github.com/amir20/dozzle/internal/utils"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/dustin/go-humanize"
	"github.com/go-chi/chi/v5"

	log "github.com/sirupsen/logrus"
)

func (h *handler) downloadLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	containerService, err := h.multiHostService.FindContainer(hostKey(r), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now()
	nowFmt := now.Format("2006-01-02T15-04-05")

	contentDisposition := fmt.Sprintf("attachment; filename=%s-%s.log", containerService.Container.Name, nowFmt)

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Disposition", contentDisposition)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/text")
	} else {
		w.Header().Set("Content-Disposition", contentDisposition+".gz")
		w.Header().Set("Content-Type", "application/gzip")
	}

	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	zw := gzip.NewWriter(w)
	defer zw.Close()
	zw.Name = fmt.Sprintf("%s-%s.log", containerService.Container.Name, nowFmt)
	zw.Comment = "Logs generated by Dozzle"
	zw.ModTime = now

	reader, err := containerService.RawLogs(r.Context(), time.Time{}, now, stdTypes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if containerService.Container.Tty {
		io.Copy(zw, reader)
	} else {
		stdcopy.StdCopy(zw, zw, reader)
	}
}

func (h *handler) fetchLogsBetweenDates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-jsonl; charset=UTF-8")

	from, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
	to, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
	id := chi.URLParam(r, "id")

	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	containerService, err := h.multiHostService.FindContainer(hostKey(r), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	events, err := containerService.LogsBetweenDates(r.Context(), from, to, stdTypes)
	if err != nil {
		log.Errorf("error while streaming logs %v", err.Error())
	}

	buffer := utils.NewRingBuffer[*docker.LogEvent](500)

	for event := range events {
		buffer.Push(event)
	}

	encoder := json.NewEncoder(w)
	for _, event := range buffer.Data() {
		if err := encoder.Encode(event); err != nil {
			log.Errorf("json encoding error while streaming %v", err.Error())
		}
	}
}

func (h *handler) streamContainerLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	streamLogsForContainers(w, r, h.multiHostService, func(container *docker.Container) bool {
		return container.ID == id && container.Host == hostKey(r)
	})
}

func (h *handler) streamLogsMerged(w http.ResponseWriter, r *http.Request) {
	if !r.URL.Query().Has("id") {
		http.Error(w, "ids query parameter is required", http.StatusBadRequest)
		return
	}

	ids := make(map[string]bool)
	for _, id := range r.URL.Query()["id"] {
		ids[id] = true
	}

	streamLogsForContainers(w, r, h.multiHostService, func(container *docker.Container) bool {
		return ids[container.ID] && container.Host == hostKey(r)
	})
}

func (h *handler) streamServiceLogs(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	streamLogsForContainers(w, r, h.multiHostService, func(container *docker.Container) bool {
		return container.State == "running" && container.Labels["com.docker.swarm.service.name"] == service
	})
}

func (h *handler) streamGroupedLogs(w http.ResponseWriter, r *http.Request) {
	group := chi.URLParam(r, "group")

	streamLogsForContainers(w, r, h.multiHostService, func(container *docker.Container) bool {
		return container.State == "running" && container.Group == group
	})
}

func (h *handler) streamStackLogs(w http.ResponseWriter, r *http.Request) {
	stack := chi.URLParam(r, "stack")

	streamLogsForContainers(w, r, h.multiHostService, func(container *docker.Container) bool {
		return container.State == "running" && container.Labels["com.docker.stack.namespace"] == stack
	})
}

func streamLogsForContainers(w http.ResponseWriter, r *http.Request, multiHostClient *MultiHostService, filter ContainerFilter) {
	var stdTypes docker.StdType
	if r.URL.Query().Has("stdout") {
		stdTypes |= docker.STDOUT
	}
	if r.URL.Query().Has("stderr") {
		stdTypes |= docker.STDERR
	}

	if stdTypes == 0 {
		http.Error(w, "stdout or stderr is required", http.StatusBadRequest)
		return
	}

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-transform")
	w.Header().Add("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	logs := make(chan *docker.LogEvent)
	events := make(chan *docker.ContainerEvent, 1)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	existingContainers, errs := multiHostClient.ListAllContainersFiltered(filter)
	if len(errs) > 0 {
		log.Warnf("error while listing containers %v", errs)
	}

	streamLogs := func(container docker.Container) {
		containerService, err := multiHostClient.FindContainer(container.Host, container.ID)
		if err != nil {
			log.Errorf("error while finding container %v", err.Error())
			return
		}
		err = containerService.StreamLogs(r.Context(), container.StartedAt, stdTypes, logs)
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.WithError(err).Debugf("stream closed for container %v", container.Name)
				events <- &docker.ContainerEvent{ActorID: container.ID, Name: "container-stopped", Host: container.Host}
			} else if !errors.Is(err, context.Canceled) {
				log.Errorf("unknown error while streaming %v", err.Error())
			}
		}
	}

	for _, container := range existingContainers {
		go streamLogs(container)
	}

	newContainers := make(chan docker.Container)
	multiHostClient.SubscribeContainersStarted(r.Context(), newContainers, filter)

loop:
	for {
		select {
		case event := <-logs:
			if buf, err := json.Marshal(event); err != nil {
				log.Errorf("json encoding error while streaming %v", err.Error())
			} else {
				fmt.Fprintf(w, "data: %s\n", buf)
			}
			if event.Timestamp > 0 {
				fmt.Fprintf(w, "id: %d\n", event.Timestamp)
			}
			fmt.Fprintf(w, "\n")
			f.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ":ping \n\n")
			f.Flush()
		case container := <-newContainers:
			events <- &docker.ContainerEvent{ActorID: container.ID, Name: "container-started", Host: container.Host}
			go streamLogs(container)

		case event := <-events:
			log.Debugf("received container event %v", event)
			if buf, err := json.Marshal(event); err != nil {
				log.Errorf("json encoding error while streaming %v", err.Error())
			} else {
				fmt.Fprintf(w, "event: container-event\ndata: %s\n\n", buf)
				f.Flush()
			}

		case <-r.Context().Done():
			log.Debugf("context cancelled")
			break loop
		}
	}

	if log.IsLevelEnabled(log.DebugLevel) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.WithFields(log.Fields{
			"allocated":      humanize.Bytes(m.Alloc),
			"totalAllocated": humanize.Bytes(m.TotalAlloc),
			"system":         humanize.Bytes(m.Sys),
			"routines":       runtime.NumGoroutine(),
		}).Debug("runtime mem stats")
	}
}
