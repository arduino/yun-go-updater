#!/bin/bash -xe

mkdir -p distrib/{linux32,linux64,linuxarm,windows,osx}/{tftp,avr}

export GOPATH=$PWD

u_boot_fw=u-boot-arduino-lede.bin
sysupgrade_fw_name=ledeyun-17.11-r5405-3cd7761-ar71xx-generic-arduino-yun-squashfs-sysupgrade.bin

#check that sources match the real filename
grep $sysupgrade_fw_name main.go
grep $u_boot_fw main.go

#Linux32
CGO_ENABLED=0 GOOS=linux GOARCH=386 GO386=387 go build -o distrib/linux32/yun-go-updater
cp tftp/{$sysupgrade_fw_name,$u_boot_fw} distrib/linux32/tftp
cp avr/*.hex distrib/linux32/avr/
cd distrib/linux32/avr/
wget http://downloads.arduino.cc/tools/avrdude-6.3.0-arduino8-i686-pc-linux-gnu.tar.bz2
tar xvf *.tar.bz2
rm -rf *.tar.bz2
mv avrdude/{bin,etc} .
rm -rf avrdude
cd -

#Linux64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o distrib/linux64/yun-go-updater
cp tftp/{$sysupgrade_fw_name,$u_boot_fw} distrib/linux64/tftp
cp avr/*.hex distrib/linux64/avr/
cd distrib/linux64/avr/
wget http://downloads.arduino.cc/tools/avrdude-6.3.0-arduino8-x86_64-pc-linux-gnu.tar.bz2
tar xvf *.tar.bz2
rm -rf *.tar.bz2
mv avrdude/{bin,etc} .
rm -rf avrdude
cd -

#LinuxARM
CGO_ENABLED=0 GOOS=linux GOARCH=arm go build -o distrib/linuxarm/yun-go-updater
cp tftp/{$sysupgrade_fw_name,$u_boot_fw} distrib/linuxarm/tftp
cp avr/*.hex distrib/linuxarm/avr/
cd distrib/linuxarm/avr/
wget http://downloads.arduino.cc/tools/avrdude-6.3.0-arduino8-armhf-pc-linux-gnu.tar.bz2
tar xvf *.tar.bz2
rm -rf *.tar.bz2
mv avrdude/{bin,etc} .
rm -rf avrdude
cd -

#Windows
CGO_ENABLED=0 GOOS=windows GOARCH=386 GO386=387 go build -o distrib/windows/yun-go-updater
cp tftp/{$sysupgrade_fw_name,$u_boot_fw} distrib/windows/tftp
cp avr/*.hex distrib/windows/avr/
cd distrib/windows/avr/
wget http://downloads.arduino.cc/tools/avrdude-6.3.0-arduino8-i686-w64-mingw32.zip
unzip avrdude-6.3.0-arduino8-i686-w64-mingw32.zip
rm -rf *.zip
mv avrdude/{bin,etc} .
rm -rf avrdude
cd -

#OSX
CGO_ENABLED=0 GOOS=linux GOARCH=arm go build -o distrib/osx/yun-go-updater
cp tftp/{$sysupgrade_fw_name,$u_boot_fw} distrib/osx/tftp
cp avr/*.hex distrib/osx/avr/
cd distrib/osx/avr/
wget http://downloads.arduino.cc/tools/avrdude-6.3.0-arduino8-i386-apple-darwin11.tar.bz2
tar xvf *.tar.bz2
rm -rf *.tar.bz2
mv avrdude/{bin,etc} .
rm -rf avrdude
cd -
