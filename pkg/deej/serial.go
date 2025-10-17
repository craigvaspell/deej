package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// Connection state
	connectErrorChannel chan error
	stopChannel         chan bool
	connected           bool
	conn                serial.Port

	// Connection config
	autoDetectPort bool
	baudRate       int
	portName       string
	readTimeout    time.Duration
	reconnectDelay time.Duration

	// Slider state / config
	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		stopChannel:         make(chan bool),
		connected:           false,
		conn:                nil,
		sliderMoveConsumers: []chan SliderMoveEvent{},
		readTimeout:         2 * time.Second,
		reconnectDelay:      3 * time.Second,
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to our arduino chip
func (sio *SerialIO) Start() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	configPort := sio.deej.config.ConnectionInfo.COMPort
	sio.autoDetectPort = (configPort == "")
	sio.baudRate = sio.deej.config.ConnectionInfo.BaudRate

	if sio.autoDetectPort {
		sio.logger.Info("Explicit COM port not specified, will auto-detect")
	} else {
		sio.portName = configPort
		sio.logger.Debugw("Using explicit configured COM port", "port", sio.portName)
	}

	// Initialize channel for reporting connection state
	sio.connectErrorChannel = make(chan error, 1)

	// Kick off main loop
	go sio.readLoop()

	// Report initial startup state to invoker
	err := <-sio.connectErrorChannel
	return err
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.stopChannel <- true
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

func (sio *SerialIO) GetConnectedPort() string {
	if sio.connected {
		return sio.portName
	}
	return ""
}

func (sio *SerialIO) connect() error {
	if sio.autoDetectPort {
		detectedPort, err := sio.detectDeejPort()
		if err != nil {
			sio.logger.Warnw("Auto-detect of Deej port failed", "error", err)
			return fmt.Errorf("auto-detect Deej: %w", err)
		}
		sio.portName = detectedPort
		sio.logger.Infow("Auto-detect of Deej port successful", "port", sio.portName)
	}

	mode := &serial.Mode{
		BaudRate: sio.baudRate,
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	}

	sio.logger.Debugw(
		"Opening serial connection",
		"port", sio.portName,
		"baudRate", sio.baudRate)

	port, err := serial.Open(sio.portName, mode)
	if err != nil {
		sio.logger.Warnw("Failed to open serial port", "port", sio.portName)
		return fmt.Errorf("open serial port: %w", err)
	}

	if err := port.SetReadTimeout(sio.readTimeout); err != nil {
		port.Close()
		sio.logger.Warnw("Failed to set read timeout", "error", err)
		return fmt.Errorf("set read timeout: %w", err)
	}

	sio.conn = port
	sio.connected = true
	sio.logger.Infow("Serial connection established", "port", sio.portName)

	time.Sleep(100 * time.Millisecond)
	port.ResetInputBuffer()

	return nil
}

func (sio *SerialIO) disconnect() {
	if sio.conn != nil {
		if err := sio.conn.Close(); err != nil {
			sio.logger.Warnw("Failed to close serial connection", "error", err)
		} else {
			sio.logger.Debug("Serial connection closed")
		}
		sio.conn = nil
	}
	sio.connected = false
}

func (sio *SerialIO) detectDeejPort() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return "", err
	}

	if len(ports) == 0 {
		return "", errors.New("no serial ports found")
	}

	var candidates []string

	for _, port := range ports {
		if sio.deej.Verbose() {
			sio.logger.Debugw(
				"Found port",
				"name", port.Name,
				"vid", port.VID,
				"pid", port.PID,
				"isUSB", port.IsUSB)
		}

		if port.IsUSB {
			switch port.VID {
			case "2341", "2A03", "1a86", "0403": // Arduino, Arduino LLC CH340, FTDI
				candidates = append(candidates, port.Name)
				sio.logger.Debugw(
					"Found Arduino compatible device",
					"port", port.Name,
					"vid", port.VID)
			}
		}
	}

	for _, portName := range candidates {
		if sio.verifyDeejPort(portName) {
			return portName, nil
		}
	}

	sio.logger.Debug("No Deej found on ports with Arduino compatible VIDs, trying all ports...")
	for _, port := range ports {
		if sio.verifyDeejPort(port.Name) {
			return port.Name, nil
		}
	}

	return "", errors.New("no Deej found on any port")
}

func (sio *SerialIO) verifyDeejPort(portName string) bool {
	sio.logger.Debugw("Testing port for presence of Deej", "port", portName)

	mode := &serial.Mode{
		BaudRate: sio.baudRate,
		Parity:   serial.NoParity,
		DataBits: 8,
		StopBits: serial.OneStopBit,
	}

	port, err := serial.Open(portName, mode)
	if err != nil {
		if sio.deej.Verbose() {
			sio.logger.Debugw(
				"Failed to open port for verification",
				"port", portName,
				"error", err)
		}
		return false
	}
	defer port.Close()

	port.SetReadTimeout(1 * time.Second)

	reader := bufio.NewReader(port)
	attempts := 0
	maxAttempts := 5
	verifiedId := false

	for attempts < maxAttempts {
		line, err := reader.ReadString('\n')
		if err != nil {
			attempts++
			continue
		}

		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "ID:DEEJ_SAFETY_DANCE") {
			sio.logger.Debugw(
				"Found attached device with Deej ID",
				"port", portName,
				"id", line)
			verifiedId = true
			continue
		}

		parts := strings.Split(line, "|")
		if len(parts) > 0 {
			valid := true
			for _, part := range parts {
				val, err := strconv.Atoi(strings.TrimSpace(part))
				if err != nil || val < 0 || val > 1023 {
					valid = false
					break
				}
			}

			if valid {
				sio.logger.Debugw(
					"Found valid Deej data",
					"port", portName)
				return true
			}
		}

		attempts++
	}

	if verifiedId {
		sio.logger.Debugw(
			"Found attached device with Deej ID but no data yet",
			"port", portName)
		return true
	}

	return false
}

func (sio *SerialIO) readLoop() {
	var reader *bufio.Reader
	var namedLogger *zap.SugaredLogger

	for {
		select {
		case <-sio.stopChannel:
			sio.disconnect()
			return

		default:
			if !sio.connected {
				sio.logger.Debugw("Attempting to connect", "delay", sio.reconnectDelay)
				time.Sleep(sio.reconnectDelay)

				if err := sio.connect(); err != nil {
					sio.logger.Warnw("Connection failed", "error", err)
					continue
				}

				namedLogger = sio.logger.Named(strings.ToLower(sio.portName))
				reader = bufio.NewReader(sio.conn)

				namedLogger.Info("Connection successful")
				sio.deej.notifier.Notify(
					"Deej connected!",
					fmt.Sprintf("Connected to Deej on port %s", sio.portName))
			}
		}

		if reader == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "timeout") {
				if sio.deej.Verbose() {
					sio.logger.Debugw("Read timeout or EOF", "error", err)
				}
				continue
			}

			sio.logger.Warnw("Serial read error, will reconnect", "error", err)
			sio.disconnect()
			continue
		}

		sio.handleLine(namedLogger, line)
	}
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	line = strings.TrimSpace(line)
	if line == "" {
		return
	}

	if strings.HasPrefix(line, "ID:") {
		if sio.deej.Verbose() {
			logger.Debugw("Received device ID", "id", line)
		}
		return
	}

	// split on pipe (|), this gives a slice of numerical strings between "0" and "1023"
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, err := strconv.Atoi(strings.TrimSpace(stringValue))
		if err != nil {
			if sio.deej.Verbose() {
				logger.Debugw(
					"Failed to parse slider value",
					"index", sliderIdx,
					"value", stringValue,
					"error", err)
			}
			return
		}

		// validate range
		if number < 0 || number > 1023 {
			if sio.deej.Verbose() {
				logger.Debugw(
					"Slider value out of range",
					"index", sliderIdx,
					"value", number)
			}
			return
		}

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / 1023.0

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if sio.deej.config.InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for range configReloadedChannel {
			// make any config reload unset our slider number to ensure process volumes are being re-set
			// (the next read line will emit SliderMoveEvent instances for all sliders)\
			// this needs to happen after a small delay, because the session map will also re-acquire sessions
			// whenever the config file is reloaded, and we don't want it to receive these move events while the map
			// is still cleared. this is kind of ugly, but shouldn't cause any issues
			go func() {
				<-time.After(stopDelay)
				sio.lastKnownNumSliders = 0
			}()

			// if connection params have changed, attempt to stop and start the connection
			newPort := sio.deej.config.ConnectionInfo.COMPort
			newBaudRate := sio.deej.config.ConnectionInfo.BaudRate
			newAutoDetect := (newPort == "")

			needReconnect := false
			if newAutoDetect != sio.autoDetectPort {
				sio.logger.Info("Auto-detect mode changed, will reconnect")
				needReconnect = true
			} else if !newAutoDetect && newPort != sio.portName {
				sio.logger.Infow(
					"COM port changed, will reconnect",
					"old", sio.portName,
					"new", newPort)
				needReconnect = true
			} else if newBaudRate != sio.baudRate {
				sio.logger.Infow(
					"Baud rate changed, will reconnect",
					"old", sio.baudRate,
					"new", newBaudRate)
				needReconnect = true
			}

			if needReconnect {
				sio.Stop()
				<-time.After(stopDelay)

				sio.portName = newPort
				sio.baudRate = newBaudRate
				sio.autoDetectPort = newAutoDetect

				if err := sio.Start(); err != nil {
					sio.logger.Warnw("Failed to reconnect after config change", "error", err)
				} else {
					sio.logger.Info("Reconnnected successfully after config change")
				}
			}
		}
	}()
}
