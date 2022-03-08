import http.client, json, requests, time

def submit_app(clientId, clientSecret, tenantId):
    # Get token

    tokenEndpoint = "https://login.microsoftonline.com/{0}/oauth2/token"
    tokenResource = "https://manage.devcenter.microsoft.com"

    tokenRequestBody = "grant_type=client_credentials&client_id={0}&client_secret={1}&resource={2}".format(clientId, clientSecret, tokenResource)
    headers = {"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"}
    tokenConnection = http.client.HTTPSConnection("login.microsoftonline.com")
    tokenConnection.request("POST", "/{0}/oauth2/token".format(tenantId), tokenRequestBody, headers=headers)

    tokenResponse = tokenConnection.getresponse()
    print(tokenResponse.status)
    tokenJson = json.loads(tokenResponse.read().decode())
    print(tokenJson["access_token"])

    tokenConnection.close()

    # create a new submission

    accessToken = tokenJson["access_token"]  # Your access token
    applicationId = "ZitiDesktopEdge"  # Your application ID
    appSubmissionRequestJson = ""  # Your submission request JSON
    zipFilePath = r'*.zip'  # Your zip file path

    headers = {"Authorization": "Bearer " + accessToken,
            "Content-type": "application/json",
            "User-Agent": "Python"}
    ingestionConnection = http.client.HTTPSConnection("manage.devcenter.microsoft.com")

    # Get application
    ingestionConnection.request("GET", "/v1.0/my/applications/{0}".format(applicationId), "", headers)
    appResponse = ingestionConnection.getresponse()
    print(appResponse.status)
    print(appResponse.headers["MS-CorrelationId"])  # Log correlation ID

    # Delete existing in-progress submission
    appJsonObject = json.loads(appResponse.read().decode())
    if "pendingApplicationSubmission" in appJsonObject :
        submissionToRemove = appJsonObject["pendingApplicationSubmission"]["id"]
        ingestionConnection.request("DELETE", "/v1.0/my/applications/{0}/submissions/{1}".format(applicationId, submissionToRemove), "", headers)
        deleteSubmissionResponse = ingestionConnection.getresponse()
        print(deleteSubmissionResponse.status)
        print(deleteSubmissionResponse.headers["MS-CorrelationId"])  # Log correlation ID
        deleteSubmissionResponse.read()

    # Create submission
    ingestionConnection.request("POST", "/v1.0/my/applications/{0}/submissions".format(applicationId), "", headers)
    createSubmissionResponse = ingestionConnection.getresponse()
    print(createSubmissionResponse.status)
    print(createSubmissionResponse.headers["MS-CorrelationId"])  # Log correlation ID

    submissionJsonObject = json.loads(createSubmissionResponse.read().decode())
    submissionId = submissionJsonObject["id"]
    fileUploadUrl = submissionJsonObject["fileUploadUrl"]
    print(submissionId)
    print(fileUploadUrl)

    # Update submission
    ingestionConnection.request("PUT", "/v1.0/my/applications/{0}/submissions/{1}".format(applicationId, submissionId), appSubmissionRequestJson, headers)
    updateSubmissionResponse = ingestionConnection.getresponse()
    print(updateSubmissionResponse.status)
    print(updateSubmissionResponse.headers["MS-CorrelationId"])  # Log correlation ID
    updateSubmissionResponse.read()

    # Upload images and packages in a zip file. Note that large file might need to be handled differently
    f = open(zipFilePath, 'rb')
    uploadResponse = requests.put(fileUploadUrl.replace("+", "%2B"), f, headers={"x-ms-blob-type": "BlockBlob"})
    print(uploadResponse.status_code)

    # Commit submission
    ingestionConnection.request("POST", "/v1.0/my/applications/{0}/submissions/{1}/commit".format(applicationId, submissionId), "", headers)
    commitResponse = ingestionConnection.getresponse()
    print(commitResponse.status)
    print(commitResponse.headers["MS-CorrelationId"])  # Log correlation ID
    print(commitResponse.read())

    # Pull submission status until commit process is completed
    ingestionConnection.request("GET", "/v1.0/my/applications/{0}/submissions/{1}/status".format(applicationId, submissionId), "", headers)
    getSubmissionStatusResponse = ingestionConnection.getresponse()
    submissionJsonObject = json.loads(getSubmissionStatusResponse.read().decode())
    while submissionJsonObject["status"] == "CommitStarted":
        time.sleep(60)
        ingestionConnection.request("GET", "/v1.0/my/applications/{0}/submissions/{1}/status".format(applicationId, submissionId), "", headers)
        getSubmissionStatusResponse = ingestionConnection.getresponse()
        submissionJsonObject = json.loads(getSubmissionStatusResponse.read().decode())
        print(submissionJsonObject["status"])

    print(submissionJsonObject["status"])
    print(submissionJsonObject)

    ingestionConnection.close()

def prepare_zip(installerVersion):
    encoding = sys.getfilesystemencoding()
    dir_path = os.path.dirname(unicode(sys.executable, encoding))
    file_name = "Ziti Desktop Edge Client-{0}.zip".format(installerVersion)
    new_file = os.path.join(dir_path, file_name)
    # creating zip file with write mode
    zip = zipfile.ZipFile(new_file, 'w', zipfile.ZIP_DEFLATED)
    # Walk through the files in a directory
    for dir_path, dir_names, files in os.walk(dir_path):
        f_path = dir_path.replace(dir_path, '')
        f_path = f_path and f_path + os.sep
        # Writing each file into the zip
        for file in files:
            zip.write(os.path.join(dir_path, file), f_path + file)
    zip.close()
    print("File Created successfully..")
    return new_file

if __name__ == '__main__':
    tenantId = "25445e86-2ae6-4434-b116-25c66c27168d"  # Your tenant ID
    clientId = ""  # Your client ID
    clientSecret = ""  # Your client secret
    installerVersion = ""

    submit_app(clientId, clientSecret, tenantId)