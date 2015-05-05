package qemu

import (
    "net"
    "os"
    "strconv"
    "sync"
    "dvm/api/pod"
    "log"
    "dvm/api/types"
    "dvm/lib/glog"
    "time"
    "fmt"
)

type QemuContext struct {
    id  string

    cpu     int
    memory  int
    pciAddr int  //next available pci addr for pci hotplug
    scsiId  int  //next available scsi id for scsi hotplug
    attachId uint64 //next available attachId for attached tty
    kernel  string
    initrd  string

    // Communication Context
    hub chan QemuEvent
    qmp chan QmpInteraction
    vm  chan *DecodedMessage
    client chan *types.QemuResponse
    wdt chan string

    ptys        *pseudoTtys
    timer       *time.Timer
    transition  QemuEvent
    ttySessions map[string]uint64

    qmpSockName string
    dvmSockName string
    ttySockName string
    consoleSockName  string
    shareDir    string

    process     *os.Process

    qmpSock     *net.UnixListener
    dvmSock     *net.UnixListener
    ttySock     *net.UnixListener

    handler     stateHandler

    // Specification
    userSpec    *pod.UserPod
    vmSpec      *VmPod
    devices     *deviceMap
    progress    *processingList

    // Internal Helper
    lock *sync.Mutex //protect update of context
}

type deviceMap struct {
    imageMap    map[string]*imageInfo
    volumeMap   map[string]*volumeInfo
    networkMap  map[int]*InterfaceCreated
}

type blockDescriptor struct {
    name        string
    filename    string
    format      string
    fstype      string
    deviceName  string
    scsiId      int
}

type imageInfo struct {
    info        *blockDescriptor
    pos         int
}

type volumeInfo struct {
    info        *blockDescriptor
    pos         volumePosition
    readOnly    map[int]bool
}

type volumePosition map[int]string     //containerIdx -> mpoint

type processingList struct {
    adding      *processingMap
    deleting    *processingMap
    finished    *processingMap
}

type processingMap struct {
    containers  map[int]bool
    volumes     map[string]bool
    blockdevs   map[string]bool
    networks    map[int]bool
    ttys        map[int]bool
    serialPorts map[int]bool
}

type stateHandler func(ctx *QemuContext, event QemuEvent)

func newDeviceMap() *deviceMap {
    return &deviceMap{
        imageMap:   make(map[string]*imageInfo),
        volumeMap:  make(map[string]*volumeInfo),
        networkMap: make(map[int]*InterfaceCreated),
    }
}

func newProcessingMap() *processingMap{
    return &processingMap{
        containers: make(map[int]bool),    //to be create, and get images,
        volumes:    make(map[string]bool),  //to be create, and get volume
        blockdevs:  make(map[string]bool),  //to be insert to qemu, both volume and images
        networks:   make(map[int]bool),
    }
}

func newProcessingList() *processingList{
    return &processingList{
        adding:     newProcessingMap(),
        deleting:   newProcessingMap(),
        finished:   newProcessingMap(),
    }
}

func initContext(id string, hub chan QemuEvent, client chan *types.QemuResponse, cpu, memory int, kernel, initrd string) (*QemuContext,error) {

    var err error = nil

    qmpChannel := make(chan QmpInteraction, 128)
    vmChannel  := make(chan *DecodedMessage, 128)
    defer func(){ if err != nil {close(qmpChannel);close(vmChannel)}}()

    //dir and sockets:
    homeDir := BaseDir + "/" + id + "/"
    qmpSockName := homeDir + QmpSockName
    dvmSockName := homeDir + DvmSockName
    ttySockName := homeDir + TtySockName
    consoleSockName := homeDir + ConsoleSockName
    shareDir    := homeDir + ShareDir

    err = os.MkdirAll(shareDir, 0755)
    if err != nil {
        glog.Error("cannot make dir", shareDir, err.Error())
        return nil,err
    }
    defer func(){ if err != nil {os.RemoveAll(homeDir)}}()

    mkSureNotExist(qmpSockName)
    mkSureNotExist(dvmSockName)
    mkSureNotExist(ttySockName)
    mkSureNotExist(consoleSockName)

    qmpSock,err := net.ListenUnix("unix",  &net.UnixAddr{qmpSockName, "unix"})
    if err != nil {
        glog.Error("cannot create socket", qmpSockName, err.Error())
        return nil,err
    }
    defer func(){ if err != nil {qmpSock.Close()}}()

    dvmSock,err := net.ListenUnix("unix",  &net.UnixAddr{dvmSockName, "unix"})
    if err != nil {
        glog.Error("cannot create socket", dvmSockName, err.Error())
        return nil,err
    }
    defer func(){ if err != nil {dvmSock.Close()}}()

    ttySock,err := net.ListenUnix("unix",  &net.UnixAddr{ttySockName, "unix"})
    if err != nil {
        glog.Error("cannot create socket", ttySock, err.Error())
        return nil,err
    }
    defer func(){ if err != nil {ttySock.Close()}}()

    return &QemuContext{
        id:         id,
        cpu:        cpu,
        memory:     memory,
        pciAddr:    PciAddrFrom,
        scsiId:     0,
        attachId:   1,
        kernel:     kernel,
        initrd:     initrd,
        hub:        hub,
        client:     client,
        qmp:        qmpChannel,
        vm:         vmChannel,
        wdt:        make(chan string, 16),
        ptys:       newPts(),
        ttySessions: make(map[string]uint64),
        qmpSockName: qmpSockName,
        dvmSockName: dvmSockName,
        ttySockName: ttySockName,
        consoleSockName: consoleSockName,
        shareDir:   shareDir,
        timer:      nil,
        transition: nil,
        process:    nil,
        qmpSock:    qmpSock,
        dvmSock:    dvmSock,
        ttySock:    ttySock,
        handler:    stateInit,
        userSpec:   nil,
        vmSpec:     nil,
        devices:    newDeviceMap(),
        progress:   newProcessingList(),
        lock:       &sync.Mutex{},
    },nil
}

func mkSureNotExist(filename string) error {
    if _, err := os.Stat(filename); os.IsNotExist(err) {
        glog.V(1).Info("no such file: ", filename)
        return nil
    } else if err == nil {
        glog.V(1).Info("try to remove exist file", filename)
        return os.Remove(filename)
    } else {
        glog.Error("can not state file ", filename)
        return err
    }
}

func (pm *processingMap) isEmpty() bool {
    return len(pm.containers) == 0 && len(pm.volumes) == 0 && len(pm.blockdevs) == 0 &&
        len(pm.networks) == 0
}

func (ctx *QemuContext) resetAddr() {
    ctx.lock.Lock()
    ctx.pciAddr = PciAddrFrom
    ctx.scsiId = 0
    //do not reset attach id here
    ctx.lock.Unlock()
}

func (ctx* QemuContext) nextScsiId() int {
    ctx.lock.Lock()
    id := ctx.scsiId
    ctx.scsiId++
    ctx.lock.Unlock()
    return id
}

func (ctx* QemuContext) nextPciAddr() int {
    ctx.lock.Lock()
    addr := ctx.pciAddr
    ctx.pciAddr ++
    ctx.lock.Unlock()
    return addr
}

func (ctx* QemuContext) nextAttachId() uint64 {
    ctx.lock.Lock()
    id := ctx.attachId
    ctx.attachId ++
    ctx.lock.Unlock()
    return id
}

func (ctx *QemuContext) clientReg(tag string, session uint64) {
    ctx.lock.Lock()
    ctx.ttySessions[tag] = session
    ctx.lock.Unlock()
}

func (ctx *QemuContext) clientDereg(tag string) {
    if tag == "" {
        return
    }
    ctx.lock.Lock()
    if _,ok := ctx.ttySessions[tag]; ok {
        delete(ctx.ttySessions, tag)
    }
    ctx.lock.Unlock()
}

func (ctx* QemuContext) containerCreated(info *ContainerCreatedEvent) bool {
    ctx.lock.Lock()
    defer ctx.lock.Unlock()

    needInsert := false

    c := &ctx.vmSpec.Containers[info.Index]
    c.Id     = info.Id
    c.Rootfs = info.Rootfs
    c.Fstype = info.Fstype

    cmd := c.Entrypoint
    if len(c.Entrypoint) == 0 && len(info.Entrypoint) > 0 {
        cmd = info.Entrypoint
    }
    if len(c.Cmd) > 0 {
        cmd = append(cmd, c.Cmd...)
    } else if len(info.Cmd) > 0 {
        cmd = append(cmd, info.Cmd...)
    }
    c.Cmd = cmd
    c.Entrypoint = []string{}

    if c.Workdir == "" {
        c.Workdir = info.Workdir
    }
    for _,e := range c.Envs {
        if _,ok := info.Envs[e.Env]; ok {
            delete(info.Envs, e.Env)
        }
    }
    for e,v := range info.Envs {
        c.Envs = append(c.Envs, VmEnvironmentVar{Env:e, Value:v,})
    }

    if info.Fstype == "dir" {
        c.Image = info.Image
    } else {
        ctx.devices.imageMap[info.Image] = &imageInfo{
            info: &blockDescriptor{
                name: info.Image, filename: info.Image, format:"raw", fstype:info.Fstype, deviceName: "",},
            pos: info.Index,
        }
        ctx.progress.adding.blockdevs[info.Image] = true
        needInsert = true
    }

    ctx.progress.finished.containers[info.Index] = true
    delete(ctx.progress.adding.containers, info.Index)

    return needInsert
}

func (ctx* QemuContext) volumeReady(info *VolumeReadyEvent) bool {
    ctx.lock.Lock()
    defer ctx.lock.Unlock()

    needInsert := false

    vol := ctx.devices.volumeMap[info.Name]
    vol.info.filename = info.Filepath
    vol.info.format = info.Format
    vol.info.fstype = info.Fstype

    if info.Fstype != "dir" {
        ctx.progress.adding.blockdevs[info.Name] = true
        needInsert = true
    } else {
        for i,mount := range vol.pos {
            ctx.vmSpec.Containers[i].Fsmap = append(ctx.vmSpec.Containers[i].Fsmap, VmFsmapDescriptor{
                Source: info.Filepath,
                Path:   mount,
                ReadOnly: vol.readOnly[i],
            })
        }
    }

    ctx.progress.finished.volumes[info.Name] = true
    if _,ok := ctx.progress.adding.volumes[info.Name] ; ok {
        delete(ctx.progress.adding.volumes, info.Name)
    }

    return needInsert
}

func (ctx* QemuContext) blockdevInserted(info *BlockdevInsertedEvent) {
    ctx.lock.Lock()
    defer ctx.lock.Unlock()

    if info.SourceType == "image" {
        image := ctx.devices.imageMap[info.Name]
        image.info.deviceName = info.DeviceName
        image.info.scsiId     = info.ScsiId
        ctx.vmSpec.Containers[image.pos].Image = info.DeviceName
    } else if info.SourceType == "volume" {
        volume := ctx.devices.volumeMap[info.Name]
        volume.info.deviceName = info.DeviceName
        volume.info.scsiId     = info.ScsiId
        for c,vol := range volume.pos {
            ctx.vmSpec.Containers[c].Volumes = append(ctx.vmSpec.Containers[c].Volumes,
                VmVolumeDescriptor{
                    Device:info.DeviceName,
                    Mount:vol,
                    Fstype:volume.info.fstype,
                    ReadOnly:volume.readOnly[c],
                })
        }
    }

    ctx.progress.finished.blockdevs[info.Name] = true
    if _,ok := ctx.progress.adding.blockdevs[info.Name] ; ok {
        delete(ctx.progress.adding.blockdevs, info.Name)
    }
}

func (ctx *QemuContext) interfaceCreated(info* InterfaceCreated) {
    ctx.lock.Lock()
    defer ctx.lock.Unlock()
    ctx.devices.networkMap[info.Index] = info
}

func (ctx* QemuContext) netdevInserted(info *NetDevInsertedEvent) {
    ctx.lock.Lock()
    defer ctx.lock.Unlock()
    ctx.progress.finished.networks[info.Index] = true
    if _,ok := ctx.progress.adding.networks[info.Index] ; ok {
        delete(ctx.progress.adding.networks, info.Index)
    }
    if len(ctx.progress.adding.networks) == 0 {
        count := len(ctx.devices.networkMap)
        infs := make([]VmNetworkInf, count)
        routes := []VmRoute{}
        for i:=0; i < count ; i++ {
            infs[i].Device    = ctx.devices.networkMap[i].DeviceName
            infs[i].IpAddress = ctx.devices.networkMap[i].IpAddr
            infs[i].NetMask   = ctx.devices.networkMap[i].NetMask

            for _,rl := range ctx.devices.networkMap[i].RouteTable {
                dev := ""
                if rl.ViaThis {
                    dev = infs[i].Device
                }
                routes = append(routes, VmRoute{
                    Dest:       rl.Destination,
                    Gateway:    rl.Gateway,
                    Device:     dev,
                })
            }
        }
        ctx.vmSpec.Interfaces = infs
        ctx.vmSpec.Routes = routes
    }
}

func (ctx* QemuContext) deviceReady() bool {
    return ctx.progress.adding.isEmpty() && ctx.progress.deleting.isEmpty()
}

func (ctx *QemuContext) releaseVolumeDir() {
    for name,vol := range ctx.devices.volumeMap {
        if vol.info.fstype == "dir" {
            glog.V(1).Info("need umount dir ", vol.info.filename)
            ctx.progress.deleting.volumes[name] = true
            go UmountVolume(ctx.shareDir, vol.info.filename, name, ctx.hub)
        }
    }
}

func (ctx *QemuContext) removeDMDevice() {
    for name,container := range ctx.devices.imageMap {
        if container.info.fstype != "dir" {
            glog.V(1).Info("need remove dm file", container.info.filename)
            ctx.progress.deleting.blockdevs[name] = true
            go UmountDMDevice(container.info.filename, name, ctx.hub)
        }
    }
    for name,vol := range ctx.devices.volumeMap {
        if vol.info.fstype != "dir" {
            glog.V(1).Info("need remove dm file ", vol.info.filename)
            ctx.progress.deleting.blockdevs[name] = true
            go UmountDMDevice(vol.info.filename, name, ctx.hub)
        }
    }
}

func (ctx *QemuContext) releaseAufsDir() {
    for idx,container := range ctx.vmSpec.Containers {
        if container.Fstype == "dir" {
            glog.V(1).Info("need unmount aufs", container.Image)
            ctx.progress.deleting.containers[idx] = true
            go UmountAufsContainer(ctx.shareDir, container.Image, idx, ctx.hub)
        }
    }
}

func (ctx *QemuContext) removeVolumeDrive() {
    for name,vol := range ctx.devices.volumeMap {
        if vol.info.format == "raw" || vol.info.format == "qcow2" {
            glog.V(1).Infof("need detach volume %s (%s) ", name, vol.info.deviceName)
            ctx.qmp <- newDiskDelSession(ctx, vol.info.scsiId, &VolumeUnmounted{ Name: name, Success:true,})
            ctx.progress.deleting.volumes[name] = true
        }
    }
}

func (ctx *QemuContext) removeImageDrive() {
    for _,image := range ctx.devices.imageMap {
        if image.info.fstype != "dir" {
            glog.V(1).Infof("need eject no.%d image block device: %s", image.pos, image.info.deviceName)
            ctx.progress.deleting.containers[image.pos] = true
            ctx.qmp <- newDiskDelSession(ctx, image.info.scsiId, &ContainerUnmounted{ Index: image.pos, Success:true})
        }
    }
}

func (ctx* QemuContext) Lookup(container string) int {
    if container == "" {
        return -1
    }
    for idx,c := range ctx.vmSpec.Containers {
        if c.Id == container {
            glog.V(1).Infof("found container %s at %d", container, idx)
            return idx
        }
    }
    glog.V(1).Infof("can not found container %s", container)
    return -1
}

func (ctx *QemuContext) Close() {
    ctx.lock.Lock()
    defer ctx.lock.Unlock()
    close(ctx.qmp)
    close(ctx.vm)
    close(ctx.wdt)
    ctx.qmpSock.Close()
    ctx.dvmSock.Close()
    ctx.ttySock.Close()
    os.Remove(ctx.dvmSockName)
    os.Remove(ctx.qmpSockName)
    os.Remove(ctx.consoleSockName)
    os.RemoveAll(ctx.shareDir)
    ctx.handler = nil
}

func (ctx *QemuContext) Become(handler stateHandler) {
    ctx.lock.Lock()
    ctx.handler = handler
    ctx.lock.Unlock()
}

func (ctx *QemuContext) QemuArguments() []string {
    platformParams := []string{
        "-machine", "pc-i440fx-2.0,accel=kvm,usb=off", "-global", "kvm-pit.lost_tick_policy=discard", "-cpu", "host",}
    if _, err := os.Stat("/dev/kvm"); os.IsNotExist(err) {
        log.Println("kvm not exist change to no kvm mode")
        platformParams = []string{"-machine", "pc-i440fx-2.0,usb=off", "-cpu", "core2duo",}
    }
    return append(platformParams,
        "-realtime", "mlock=off", "-no-user-config", "-nodefaults", "-no-hpet",
        "-rtc", "base=utc,driftfix=slew", "-no-reboot", "-display", "none", "-boot", "strict=on",
        "-m", strconv.Itoa(ctx.memory), "-smp", strconv.Itoa(ctx.cpu),
        "-kernel", ctx.kernel, "-initrd", ctx.initrd, "-append", "\"console=ttyS0 panic=1\"",
        "-qmp", "unix:" + ctx.qmpSockName, "-serial", fmt.Sprintf("unix:%s,server,nowait", ctx.consoleSockName),
        "-device", "virtio-serial-pci,id=virtio-serial0,bus=pci.0,addr=0x2","-device", "virtio-scsi-pci,id=scsi0,bus=pci.0,addr=0x3",
        "-chardev", "socket,id=charch0,path=" + ctx.dvmSockName,
        "-device", "virtserialport,bus=virtio-serial0.0,nr=1,chardev=charch0,id=channel0,name=org.getdvm.channel.0",
        "-chardev", "socket,id=charch1,path=" + ctx.dvmSockName,
        "-device", "virtserialport,bus=virtio-serial0.0,nr=2,chardev=charch1,id=channel1,name=org.getdvm.channel.1",
        "-fsdev", fmt.Sprintf("local,id=virtio9p,path=%s,security_model=none", ctx.shareDir),
        "-device", fmt.Sprintf("virtio-9p-pci,fsdev=virtio9p,mount_tag=%s", ShareDir),
    )
}

// InitDeviceContext will init device info in context
func (ctx *QemuContext) InitDeviceContext(spec *pod.UserPod, networks int) {
    isFsmap:= make(map[string]bool)

    ctx.lock.Lock()
    defer ctx.lock.Unlock()

    for i:=0; i< networks ; i++ {
        ctx.progress.adding.networks[i] = true
    }

    //classify volumes, and generate device info and progress info
    for _,vol := range spec.Volumes {
        if vol.Source == "" {
            isFsmap[vol.Name]    = false
            ctx.devices.volumeMap[vol.Name] = &volumeInfo{
                info: &blockDescriptor{ name: vol.Name, filename: "", format:"", fstype:"", deviceName:"", },
                pos:  make(map[int]string),
                readOnly: make(map[int]bool),
            }
        } else if vol.Driver == "raw" || vol.Driver == "qcow2" {
            isFsmap[vol.Name]    = false
            ctx.devices.volumeMap[vol.Name] = &volumeInfo{
                info: &blockDescriptor{
                    name: vol.Name, filename: vol.Source, format:vol.Driver, fstype:"ext4", deviceName: "", },
                pos:  make(map[int]string),
                readOnly: make(map[int]bool),
            }
            ctx.progress.adding.blockdevs[vol.Name] = true
        } else if vol.Driver == "vfs" {
            isFsmap[vol.Name]    = true
            ctx.devices.volumeMap[vol.Name] = &volumeInfo{
                info: &blockDescriptor{
                    name: vol.Name, filename: vol.Source, format:vol.Driver, fstype:"dir", deviceName: "", },
                pos:  make(map[int]string),
                readOnly: make(map[int]bool),
            }
        }
        ctx.progress.adding.volumes[vol.Name] = true
    }

    containers := make([]VmContainer, len(spec.Containers))

    for i,container := range spec.Containers {
        vols := []VmVolumeDescriptor{}
        fsmap := []VmFsmapDescriptor{}
        for _,v := range container.Volumes {
            ctx.devices.volumeMap[v.Volume].pos[i] = v.Path
            ctx.devices.volumeMap[v.Volume].readOnly[i] = v.ReadOnly
        }

        envs := make([]VmEnvironmentVar, len(container.Envs))
        for j,e := range container.Envs {
            envs[j] = VmEnvironmentVar{ Env: e.Env, Value: e.Value,}
        }

        restart := "never"
        if len(container.RestartPolicy) > 0 {
            restart = container.RestartPolicy
        }

        containers[i] = VmContainer{
            Id:      "",   Rootfs: "rootfs", Fstype: "ext4", Image:  "",
            Volumes: vols,  Fsmap:   fsmap,   Tty:     0,
            Workdir: container.Workdir,  Entrypoint: container.Entrypoint, Cmd:     container.Command,     Envs:    envs,
            RestartPolicy: restart,
        }

        ctx.progress.adding.containers[i] = true
        if spec.Tty {
            containers[i].Tty = ctx.nextAttachId()
            ctx.ptys.ttys[containers[i].Tty] = newAttachments(i, true)
        }
    }

    ctx.vmSpec = &VmPod{
        Hostname:       spec.Name,
        Containers:     containers,
        Interfaces:     nil,
        Routes:         nil,
        Socket:         ctx.dvmSockName,
        ShareDir:       ShareDir,
    }

    ctx.userSpec = spec
}
