# 치지직 iOS OAuth 도메인 연결

`innolive.studio`의 기존 Caddy site에 `innolive-oauth.caddy`를 import하면 AASA와
치지직 callback을 landing 앞에서 직접 제공합니다. 이 설정은 API 서버 라우트가 아닙니다.

| 경로 | 응답 |
| --- | --- |
| `/.well-known/apple-app-site-association` | AASA JSON, 200, locale redirect 없음 |
| `/apple-app-site-association` | 같은 AASA JSON, 200 |
| `/auth/chzzk/callback` | 인가값을 출력하지 않는 안내 HTML, no-store |

callback은 Caddy에서 끝나므로 Next.js locale redirect와 analytics를 거치지 않습니다.
해당 경로는 `log_skip`으로 access log 전체를 생략합니다. 다른 경로의 로그에는 영향을
주지 않습니다. 안내 페이지에는 script, 외부 요청, 자동 token exchange가 없습니다.
앱이 HTTPS callback을 받으면 state 확인과 기존 `POST /auth/chzzk/connect` 호출을
담당합니다. 기존 API payload와 `CHZZK_OAUTH_REDIRECT_URI`는 변경하지 않습니다.

## 설치 범위와 확인한 구성

2026-10-01 초기 read-only 확인에서 운영 Caddy는 v2.11.4이고, 웹 site는
`innolive-web:3000`으로 proxy했습니다. 당시 Caddyfile은 컨테이너의
`/etc/caddy/Caddyfile`에 단일 파일로 bind mount되어 있습니다. 이 저장소의 API
이미지 배포는 Caddyfile이나 이 디렉토리를 자동 설치하지 않습니다. 따라서 **PR을
머지해도 운영 도메인에는 아직 적용되지 않습니다.** 호스팅 담당자가 아래 설치를
별도로 수행해야 합니다. 운영 적용 여부는 공개 URL 응답과 실행 컨테이너의 mount를
확인해야 합니다.

## 호스팅 담당자 적용 절차

1. 현재 Caddyfile과 Caddy 컨테이너/Compose 설정을 백업합니다.
2. `public/`의 두 파일을 호스트의 전용 디렉토리에 설치하고 Caddy 컨테이너 안의
   `/srv/innolive-oauth`로 read-only mount합니다. Caddy 프로세스가 읽을 수 있어야 합니다.
3. `innolive-oauth.caddy`를 별도 read-only 파일로 컨테이너의
   `/etc/caddy/innolive-oauth.caddy`에 mount합니다. Caddyfile만 단일 파일로 mount한
   구성에서는 호스트의 옆 경로에 복사하는 것만으로는 컨테이너에서 보이지 않습니다.
4. 기존 웹 site를 다음 형태로 구성합니다. 그 외 API site와 TLS/전역 설정은 보존합니다.

   ```caddyfile
   innolive.studio, www.innolive.studio {
       import /etc/caddy/innolive-oauth.caddy
       handle {
           reverse_proxy innolive-web:3000
       }
   }
   ```

   `handle`은 같은 수준에서 서로 배타적으로 실행됩니다. 특별 경로가 먼저 끝나고
   나머지만 기존 landing proxy로 갑니다. 단순한 `reverse_proxy`가 기존의 별도 `route`
   안에 먼저 실행되게 두지 않습니다. 다른 자산 경로를 쓰면 컨테이너 환경변수
   `INNOLIVE_OAUTH_ASSET_ROOT`로 지정합니다.
5. Caddy v2.8 이상(`log_skip` 지원)인지 확인하고, 실제 컨테이너의 전체 설정에
   `caddy validate --config /etc/caddy/Caddyfile`을 실행합니다. 새 mount에는 컨테이너
   재생성이 필요합니다. 승인된 운영 변경 절차에 따라 설치·재생성과 reload를 수행합니다.
6. 두 AASA URL이 HTTPS 200과 `application/json`을 반환하고 `Location`이 없는지 확인합니다.
   `applinks.details[].appIDs`와 `webcredentials.apps`가 아래 앱 ID와 일치해야 합니다.
   callback을 **실제 code 대신 가짜 query**로 호출하고 no-store/no-referrer 헤더,
   응답 및 access/runtime 로그에 query가 없는지 확인합니다. CDN/앞단 proxy가 추가되어
   있으면 그쪽도 이 callback query를 수집하지 않게 설정합니다. Caddy debug 로그나
   request dump를 사용하지 않습니다.
7. 기존 landing과 API health가 정상인지 확인합니다. 실패하면 백업한 Caddyfile과
   컨테이너 mount 구성으로 되돌린 뒤 다시 검증합니다.

## iOS 팀 연동

AASA의 `SPT4X66Z4V.com.framework.innolive`는 현재 iOS 프로젝트의 Team ID와 Bundle ID를
기준으로 합니다. 배포된 앱의 `application-identifier` entitlement에서 App ID prefix가
동일한지 앱팀이 확인해야 합니다(팀 이동/legacy prefix 앱은 Team ID와 다를 수 있습니다).
테스트 Bundle ID는 포함하지 않았습니다. `applinks`는 callback 경로만 허용합니다.
`webcredentials.apps`는 같은 앱 ID를 도메인에 연결하며,
`ASWebAuthenticationSession.Callback.https`가 HTTPS 인증 복귀를 확인하는 데 필요합니다.

앱에는 `webcredentials:innolive.studio` Associated Domains entitlement를 추가합니다.
일반 Universal Links도 처리할 경우 `applinks:innolive.studio`를 함께 추가합니다.
`applinks`만으로는 HTTPS 인증 세션의 도메인 연결 조건을 충족하지 않습니다.
`ASWebAuthenticationSession.Callback.https`의 host/path를 서버 config의 callback과
맞춥니다. 이 API는 iOS 17.4 이상에서 지원하므로 더 낮은 OS를 지원하면 복귀 방식을
별도로 정해야 합니다. 앱팀은 state mismatch를 거부한 뒤 연결 API를 호출합니다. 서버 파일 설치만으로
iOS 앱 설정이 바뀌지는 않습니다.

Apple CDN 반영 시간과 실제 iOS 인증 복귀는 설치 후 별도 확인해야 합니다.
[Apple Associated Domains](https://developer.apple.com/documentation/xcode/supporting-associated-domains),
[Auth0 Swift HTTPS callback 설정](https://github.com/auth0/Auth0.swift#add-the-associated-domain-capability),
[Caddy handle](https://caddyserver.com/docs/caddyfile/directives/handle),
[Caddy log_skip](https://caddyserver.com/docs/caddyfile/directives/log_skip)

## 로컬 검증

```sh
CADDY_BIN=/path/to/caddy python3 deploy/oauth/test_routes.py
```

Python 표준 라이브러리와 Caddy v2.8 이상만 사용합니다. 임시 로컬 Caddy를 실행해
두 AASA 응답, 정확한 앱 ID/경로와 `webcredentials`, callback 인가값 미출력, 보안 헤더, 기존 landing
경로 유지와 access/runtime 로그 미노출을 확인합니다. CI는 운영에서 확인한 v2.11.4를 사용합니다.
