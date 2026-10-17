using TypeSpec.Http;
using TypeSpec.OpenAPI;

namespace TelDrive;

@tag("Settings")
@route("/v1/settings/webdav")
@useAuth(TelDriveAuth)
namespace WebDAV {
  /** Return the dedicated WebDAV login status for the authenticated user. */
  @get
  @operationId("getWebDAVCredentials")
  op get(): JsonResponse<200, WebDAVCredentials> | ErrorResponse<401>;

  /** Set the dedicated WebDAV username and password used by rclone/Cyberduck. */
  @put
  @operationId("setWebDAVCredentials")
  op set(@body body: WebDAVCredentialsUpdate):
    | JsonResponse<200, WebDAVCredentials>
    | ErrorResponse<401>
    | ErrorResponse<409>
    | ErrorResponse<422>;

  /** Remove the WebDAV login; clients can no longer authenticate. */
  @delete
  @operationId("deleteWebDAVCredentials")
  op remove(): EmptyResponse<204> | ErrorResponse<401>;
}
