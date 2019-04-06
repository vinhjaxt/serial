package serial

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

const rxDataTimeout = 20 * time.Millisecond

type SerialPort struct {
	LogFileName  string
	fileLog      *log.Logger
	TxMu         *sync.Mutex
	RxMu         *sync.Mutex
	NeedRx       int32
	Port         io.ReadWriteCloser
	Opened       int32
	Verbose      bool
	eventMap     map[uint32]func([]byte)
	eventNextID  uint32
	eventMapLock *sync.RWMutex
	rxBuff       chan []byte
	rxData       chan string
	rxTimer      <-chan time.Time
}

// New create new instance
func New() *SerialPort {
	return &SerialPort{
		Verbose:      true,
		eventMap:     map[uint32]func([]byte){},
		eventMapLock: &sync.RWMutex{},
		eventNextID:  0,
		Opened:       0,
		TxMu:         &sync.Mutex{},
		RxMu:         &sync.Mutex{},
		fileLog:      nil,
	}
}

// Open open connection to port
func (sp *SerialPort) Open(name string, baud int, timeout ...time.Duration) error {
	if atomic.LoadInt32(&sp.Opened) == 1 {
		return errors.New(name + " already opened")
	}
	var readTimeout time.Duration
	if len(timeout) > 0 {
		readTimeout = timeout[0]
	}

	comPort, err := openPort(name, baud, readTimeout)
	if err != nil {
		return errors.New(name + ": " + err.Error())
	}

	atomic.StoreInt32(&sp.Opened, 1)
	if sp.Verbose == false {
		sp.LogFileName = fmt.Sprintf("log_serial_%d.log", time.Now().UnixNano())
		file, err := os.OpenFile(sp.LogFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err != nil {
			log.Println(err)
		}
		sp.fileLog = log.New(file, "PREFIX: ", log.Ldate|log.Ltime)
		sp.fileLog.SetPrefix(fmt.Sprintf("[%s] ", name))
	}

	sp.Port = comPort
	sp.rxBuff = make(chan []byte, 16)
	sp.rxData = make(chan string, 16)

	go sp.readSerialPort()
	go sp.processSerialPort()
	sp.log("Port %s@%d opened", name, baud)
	return nil
}

// Close close the current Serial Port.
func (sp *SerialPort) Close() error {
	sp.Println("\x1A")
	if atomic.LoadInt32(&sp.Opened) == 1 {
		atomic.StoreInt32(&sp.Opened, 0)
		close(sp.rxBuff)
		close(sp.rxData)
		sp.log("Port closed")
		return sp.Port.Close()
	}
	return nil
}

// This method prints data trough the serial port.
func (sp *SerialPort) Write(data []byte) (n int, err error) {
	if atomic.LoadInt32(&sp.Opened) != 1 {
		err = errors.New("Port is not opened")
		return
	}
	sp.TxMu.Lock()
	n, err = sp.Port.Write(data)
	sp.TxMu.Unlock()
	if err != nil {
		atomic.StoreInt32(&sp.Opened, 0)
		return
	}
	sp.log("Tx >> %s", string(data))
	return
}

// Print send data to port
func (sp *SerialPort) Print(str string) error {
	if atomic.LoadInt32(&sp.Opened) != 1 {
		return errors.New("Port is not opened")
	}
	sp.TxMu.Lock()
	_, err := sp.Port.Write([]byte(str))
	sp.TxMu.Unlock()
	if err != nil {
		atomic.StoreInt32(&sp.Opened, 0)
		return err
	}
	sp.log("Tx >> %s", str)
	return nil
}

// Println prints data to the serial port as human-readable ASCII text followed by a carriage return character
// (ASCII 13, CR, '\r') and a newline character (ASCII 10, LF, '\n').
func (sp *SerialPort) Println(str string) error {
	return sp.Print(str + "\r\n")
}

// Printf formats according to a format specifier and print data trough the serial port.
func (sp *SerialPort) Printf(format string, args ...interface{}) error {
	str := format
	if len(args) > 0 {
		str = fmt.Sprintf(format, args...)
	}
	return sp.Print(str)
}

// SendFile send a binary file trough the serial port. If EnableLog is active then this method will log file related data.
func (sp *SerialPort) SendFile(filepath string) error {
	sentBytes := 0
	q := 512
	data := []byte{}
	file, err := ioutil.ReadFile(filepath)
	if err != nil {
		return err
	}
	fileSize := len(file)
	sp.TxMu.Lock()
	defer sp.TxMu.Unlock()
	for sentBytes <= fileSize {
		if len(file[sentBytes:]) > q {
			data = file[sentBytes:(sentBytes + q)]
		} else {
			data = file[sentBytes:]
		}
		_, err := sp.Port.Write(data)
		if err != nil {
			atomic.StoreInt32(&sp.Opened, 0)
			return err
		}
		sentBytes += q
		time.Sleep(time.Millisecond * 100)
	}
	return nil
}

// WaitForRegexTimeout wait for a defined regular expression for a defined amount of time.
func (sp *SerialPort) WaitForRegexTimeout(cmd, exp string, timeout time.Duration, inits ...func() error) ([]string, error) {
	if atomic.LoadInt32(&sp.Opened) != 1 {
		return nil, errors.New("Port is not opened")
	}

	var timeExpired int32
	regExpPattern, err := regexp.Compile(exp)
	if err != nil {
		return nil, err
	}

	ret := make(chan []string, 1)
	sp.RxMu.Lock()
	atomic.StoreInt32(&sp.NeedRx, 1)

	go func() {
		sp.log(">> Waiting: \"%s\"", exp)
		result := []string{}
		lines := ""
		for atomic.LoadInt32(&timeExpired) == 0 {
			lines += <-sp.rxData
			result = regExpPattern.FindStringSubmatch(lines)
			if len(result) > 0 {
				ret <- result
				break
			}
		}
		atomic.StoreInt32(&sp.NeedRx, 0)
		sp.RxMu.Unlock()
	}()

	time.Sleep(rxDataTimeout) // sleep at least 10ms

	if cmd != "" {
		if err := sp.Println(cmd); err != nil {
			return nil, err
		}
	}

	for _, fn := range inits {
		err := fn()
		if err != nil {
			return nil, err
		}
	}

	select {
	case data := <-ret:
		sp.log(">> Matched: %q", data[0])
		return data, nil
	case <-time.After(timeout):
		atomic.StoreInt32(&timeExpired, 1)
		sp.log(">> Failed: \"%s\" \"%s\"", cmd, exp)
		return nil, fmt.Errorf("Timeout \"%s\" \"%s\"", cmd, exp)
	}
}

func (sp *SerialPort) readSerialPort() {
	rxBuff := make([]byte, 256)
	for atomic.LoadInt32(&sp.Opened) == 1 {
		n, err := sp.Port.Read(rxBuff)
		if err != nil {
			if err != nil {
				if err != io.EOF {
					atomic.StoreInt32(&sp.Opened, 0)
					log.Println(err)
					return
				}
			}
		}
		if n > 0 {
			sp.rxBuff <- append([]byte(nil), rxBuff[:n]...)
		}
	}
}

// DelOutputListener remove func from map
func (sp *SerialPort) DelOutputListener(id uint32) {
	sp.eventMapLock.Lock()
	defer sp.eventMapLock.Unlock()
	delete(sp.eventMap, id)
}

// AddOutputListener add func to capture ouput
func (sp *SerialPort) AddOutputListener(fn func([]byte)) uint32 {
	sp.eventMapLock.Lock()
	defer sp.eventMapLock.Unlock()
	id := atomic.AddUint32(&sp.eventNextID, 1)
	sp.eventMap[id] = fn
	return id
}

func (sp *SerialPort) processSerialPort() {
	defer func() {
		if r := recover(); r != nil {
			log.Panicln(r)
		}
	}()
	var screenBuff []byte
	var rxByte []byte
	for atomic.LoadInt32(&sp.Opened) == 1 {
		select {
		case rxByte = <-sp.rxBuff:
			screenBuff = append(screenBuff, rxByte...)
			sp.rxTimer = time.After(rxDataTimeout)
			break
		case <-sp.rxTimer:
			if screenBuff == nil {
				break
			}
			sp.eventMapLock.RLock()
			for _, fn := range sp.eventMap {
				sp.eventMapLock.RUnlock()
				go fn(screenBuff)
				sp.eventMapLock.RLock()
			}
			sp.eventMapLock.RUnlock()
			sp.log("Rx << %q", screenBuff)
			if atomic.LoadInt32(&sp.NeedRx) == 1 {
				sp.rxData <- string(screenBuff)
			}
			screenBuff = nil //Clean buffer
			break
		}
	}
}

func (sp *SerialPort) log(format string, a ...interface{}) {
	if sp.Verbose {
		log.Printf(format, a...)
	} else if sp.fileLog != nil {
		sp.fileLog.Printf(format, a...)
	}
}
