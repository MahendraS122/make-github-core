package main

import "strings"

var graphTypes = map[string]string{
	"Query": "viewer:User repository:Repository node:Node", "Mutation": "createIssue:CreateIssuePayload closeIssue:CloseIssuePayload reopenIssue:ReopenIssuePayload addComment:AddCommentPayload createPullRequest:CreatePullRequestPayload mergePullRequest:MergePullRequestPayload",
	"Repository":  "id:ID databaseId:Int name:String nameWithOwner:String owner:User defaultBranchRef:Ref viewerPermission:RepositoryPermission hasIssuesEnabled:Boolean issue:Issue issues:IssueConnection pullRequests:PullRequestConnection pullRequest:PullRequest description:String url:URI isPrivate:Boolean isFork:Boolean isArchived:Boolean createdAt:DateTime updatedAt:DateTime",
	"Issue":       "id:ID databaseId:Int number:Int title:String state:IssueState body:String bodyText:String author:User createdAt:DateTime updatedAt:DateTime closedAt:DateTime closed:Boolean locked:Boolean comments:IssueCommentConnection labels:LabelConnection",
	"PullRequest": "id:ID databaseId:Int number:Int title:String body:String headRefName:String baseRefName:String headRefOid:GitObjectID baseRefOid:GitObjectID headRef:Ref baseRef:Ref mergeable:MergeableState mergeStateStatus:MergeStateStatus state:PullRequestState merged:Boolean files:PullRequestChangedFileConnection mergedAt:DateTime mergeCommit:Commit closed:Boolean closedAt:DateTime isDraft:Boolean author:User createdAt:DateTime updatedAt:DateTime",
	"User":        "id:ID databaseId:Int login:String name:String url:URI avatarUrl:URI", "Ref": "name:String target:GitObject prefix:String", "Commit": "id:ID oid:GitObjectID", "IssueComment": "id:ID databaseId:Int author:User body:String createdAt:DateTime updatedAt:DateTime url:URI", "Label": "id:ID name:String color:String description:String", "PullRequestChangedFile": "path:String additions:Int deletions:Int changeType:PatchStatus", "PageInfo": "hasNextPage:Boolean hasPreviousPage:Boolean startCursor:String endCursor:String",
	"CreateIssuePayload": "clientMutationId:String issue:Issue", "CloseIssuePayload": "clientMutationId:String issue:Issue", "ReopenIssuePayload": "clientMutationId:String issue:Issue", "AddCommentPayload": "clientMutationId:String commentEdge:IssueCommentEdge", "CreatePullRequestPayload": "clientMutationId:String pullRequest:PullRequest", "MergePullRequestPayload": "clientMutationId:String pullRequest:PullRequest",
}
var graphEnums = map[string]string{"SearchType": "ISSUE REPOSITORY USER DISCUSSION", "IssueState": "OPEN CLOSED", "PullRequestState": "OPEN CLOSED MERGED", "MergeableState": "MERGEABLE CONFLICTING UNKNOWN", "MergeStateStatus": "BEHIND BLOCKED CLEAN DIRTY DRAFT HAS_HOOKS UNKNOWN UNSTABLE", "RepositoryPermission": "ADMIN MAINTAIN WRITE TRIAGE READ", "IssueOrderField": "CREATED_AT UPDATED_AT COMMENTS", "OrderDirection": "ASC DESC", "PullRequestMergeMethod": "MERGE SQUASH REBASE", "PatchStatus": "ADDED CHANGED COPIED DELETED MODIFIED RENAMED"}

func init() {
	for _, kind := range []string{"Issue", "PullRequest", "IssueComment", "PullRequestChangedFile", "Label"} {
		graphTypes[kind+"Connection"] = "nodes:[" + kind + "] edges:[" + kind + "Edge] totalCount:Int pageInfo:PageInfo"
		graphTypes[kind+"Edge"] = "cursor:String node:" + kind
	}
}
func graphKind(name string) string {
	if _, ok := graphTypes[name]; ok {
		return "OBJECT"
	}
	if _, ok := graphEnums[name]; ok {
		return "ENUM"
	}
	if name == "Node" || name == "GitObject" || name == "Actor" {
		return "INTERFACE"
	}
	if strings.Contains("|ID|Int|String|Boolean|URI|DateTime|GitObjectID|Float|", "|"+name+"|") {
		return "SCALAR"
	}
	return ""
}
func introspectionType(name string) gqlObject {
	if inner, ok := strings.CutPrefix(name, "["); ok {
		return gql("__Type", record{"kind": "LIST", "name": nil, "ofType": introspectionType(strings.TrimSuffix(inner, "]"))})
	}
	return gql("__Type", record{"kind": graphKind(name), "name": name, "ofType": nil})
}
func introspect(name string) any {
	kind := graphKind(name)
	if kind == "" {
		return nil
	}
	var fields, enum any
	if kind == "OBJECT" {
		list := []gqlObject{}
		for field := range strings.FieldsSeq(graphTypes[name]) {
			key, typ, _ := strings.Cut(field, ":")
			list = append(list, gql("__Field", record{"name": key, "description": nil, "type": introspectionType(typ), "args": []gqlObject{}, "isDeprecated": false, "deprecationReason": nil}))
		}
		fields = list
	}
	if kind == "ENUM" {
		list := []gqlObject{}
		for value := range strings.FieldsSeq(graphEnums[name]) {
			list = append(list, gql("__EnumValue", record{"name": value, "description": nil, "isDeprecated": false, "deprecationReason": nil}))
		}
		enum = list
	}
	return gql("__Type", record{"name": name, "kind": kind, "description": nil, "fields": fields, "enumValues": enum, "inputFields": nil, "interfaces": []gqlObject{}, "possibleTypes": nil, "ofType": nil})
}
