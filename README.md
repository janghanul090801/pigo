# Pigo

**파이썬은 Golang 의 신진문물을 받아들일 필요가 있다**

## 목차
- [소개](#소개)
- [설치](#설치)
- [사용법](#사용법)
- [기술 스택](#기술-스택)
- [라이선스](#라이선스)

## 소개
python pip 는 저급하므로 Golang 의 module 시스템을 받아들일 필요가 있습니다.

## 설치(예정)
```bash
go install github.com/janghanul090801/pigo
```

## 사용법(예정)
### init
```bash
pigo init
```
프로젝트에 requirements.txt 와 .venv 를 세팅합니다. \
requirements.txt 가 있을 경우 덮어 쓰지 않습니다.

### install
```bash
pigo install [option]
```
가상환경에 패키지를 설치합니다. [option] 은 pip 과 100% 호환됩니다.
requirements.txt 를 자동으로 업데이트 합니다.

### uninstall
```bash
pigo uninstall [option]
```
가상환경에 패키지를 삭제합니다. [option] 은 pip 과 100% 호환됩니다.
requirements.txt 를 자동으로 업데이트 합니다.

### tidy
```bash
pigo tidy [path]
```
path(default='./') 에 있는 .py 파일을 탐색하여 사용하지 않는 의존성을 requirements.txt 에서 제거합니다.

### run
```bash
pigo run [pythonFile]
```
가상환경 python interpreter 를 사용하여 해당 파일을 실행합니다.

## 기술 스택
- Go

## 라이선스
MIT
