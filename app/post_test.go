// Copyright (c) 2017-present Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dyatlov/go-opengraph/opengraph"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/store"
	"github.com/mattermost/mattermost-server/store/storetest"
)

func TestUpdatePostEditAt(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	post := &model.Post{}
	*post = *th.BasicPost

	post.IsPinned = true
	if saved, err := th.App.UpdatePost(post, true); err != nil {
		t.Fatal(err)
	} else if saved.EditAt != post.EditAt {
		t.Fatal("shouldn't have updated post.EditAt when pinning post")

		*post = *saved
	}

	time.Sleep(time.Millisecond * 100)

	post.Message = model.NewId()
	if saved, err := th.App.UpdatePost(post, true); err != nil {
		t.Fatal(err)
	} else if saved.EditAt == post.EditAt {
		t.Fatal("should have updated post.EditAt when updating post message")
	}

	time.Sleep(time.Millisecond * 200)
}

func TestUpdatePostTimeLimit(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	post := &model.Post{}
	*post = *th.BasicPost

	th.App.SetLicense(model.NewTestLicense())

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.PostEditTimeLimit = -1
	})
	if _, err := th.App.UpdatePost(post, true); err != nil {
		t.Fatal(err)
	}

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.PostEditTimeLimit = 1000000000
	})
	post.Message = model.NewId()
	if _, err := th.App.UpdatePost(post, true); err != nil {
		t.Fatal("should allow you to edit the post")
	}

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.PostEditTimeLimit = 1
	})
	post.Message = model.NewId()
	if _, err := th.App.UpdatePost(post, true); err == nil {
		t.Fatal("should fail on update old post")
	}

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.PostEditTimeLimit = -1
	})
}

func TestPostReplyToPostWhereRootPosterLeftChannel(t *testing.T) {
	// This test ensures that when replying to a root post made by a user who has since left the channel, the reply
	// post completes successfully. This is a regression test for PLT-6523.
	th := Setup().InitBasic()
	defer th.TearDown()

	channel := th.BasicChannel
	userInChannel := th.BasicUser2
	userNotInChannel := th.BasicUser
	rootPost := th.BasicPost

	if _, err := th.App.AddUserToChannel(userInChannel, channel); err != nil {
		t.Fatal(err)
	}

	if err := th.App.RemoveUserFromChannel(userNotInChannel.Id, "", channel); err != nil {
		t.Fatal(err)
	}

	replyPost := model.Post{
		Message:       "asd",
		ChannelId:     channel.Id,
		RootId:        rootPost.Id,
		ParentId:      rootPost.Id,
		PendingPostId: model.NewId() + ":" + fmt.Sprint(model.GetMillis()),
		UserId:        userInChannel.Id,
		CreateAt:      0,
	}

	if _, err := th.App.CreatePostAsUser(&replyPost); err != nil {
		t.Fatal(err)
	}
}

func TestPostAction(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.AllowedUntrustedInternalConnections = "localhost 127.0.0.1"
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.PostActionIntegrationRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		assert.NoError(t, err)
		assert.Equal(t, request.UserId, th.BasicUser.Id)
		assert.Equal(t, "foo", request.Context["s"])
		assert.EqualValues(t, 3, request.Context["n"])
		fmt.Fprintf(w, `{"update": {"message": "updated"}, "ephemeral_text": "foo"}`)
	}))
	defer ts.Close()

	interactivePost := model.Post{
		Message:       "Interactive post",
		ChannelId:     th.BasicChannel.Id,
		PendingPostId: model.NewId() + ":" + fmt.Sprint(model.GetMillis()),
		UserId:        th.BasicUser.Id,
		Props: model.StringInterface{
			"attachments": []*model.SlackAttachment{
				{
					Text: "hello",
					Actions: []*model.PostAction{
						{
							Integration: &model.PostActionIntegration{
								Context: model.StringInterface{
									"s": "foo",
									"n": 3,
								},
								URL: ts.URL,
							},
							Name: "action",
						},
					},
				},
			},
		},
	}

	post, err := th.App.CreatePostAsUser(&interactivePost)
	require.Nil(t, err)

	attachments, ok := post.Props["attachments"].([]*model.SlackAttachment)
	require.True(t, ok)

	require.NotEmpty(t, attachments[0].Actions)
	require.NotEmpty(t, attachments[0].Actions[0].Id)

	err = th.App.DoPostAction(post.Id, "notavalidid", th.BasicUser.Id)
	require.NotNil(t, err)
	assert.Equal(t, http.StatusNotFound, err.StatusCode)

	err = th.App.DoPostAction(post.Id, attachments[0].Actions[0].Id, th.BasicUser.Id)
	require.Nil(t, err)
}

func TestPostChannelMentions(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	channel := th.BasicChannel
	user := th.BasicUser

	channelToMention, err := th.App.CreateChannel(&model.Channel{
		DisplayName: "Mention Test",
		Name:        "mention-test",
		Type:        model.CHANNEL_OPEN,
		TeamId:      th.BasicTeam.Id,
	}, false)
	if err != nil {
		t.Fatal(err.Error())
	}
	defer th.App.PermanentDeleteChannel(channelToMention)

	_, err = th.App.AddUserToChannel(user, channel)
	require.Nil(t, err)

	post := &model.Post{
		Message:       fmt.Sprintf("hello, ~%v!", channelToMention.Name),
		ChannelId:     channel.Id,
		PendingPostId: model.NewId() + ":" + fmt.Sprint(model.GetMillis()),
		UserId:        user.Id,
		CreateAt:      0,
	}

	result, err := th.App.CreatePostAsUser(post)
	require.Nil(t, err)
	assert.Equal(t, map[string]interface{}{
		"mention-test": map[string]interface{}{
			"display_name": "Mention Test",
		},
	}, result.Props["channel_mentions"])

	post.Message = fmt.Sprintf("goodbye, ~%v!", channelToMention.Name)
	result, err = th.App.UpdatePost(post, false)
	require.Nil(t, err)
	assert.Equal(t, map[string]interface{}{
		"mention-test": map[string]interface{}{
			"display_name": "Mention Test",
		},
	}, result.Props["channel_mentions"])
}

func TestPreparePostForClient(t *testing.T) {
	setup := func() *TestHelper {
		th := Setup().InitBasic()

		th.App.UpdateConfig(func(cfg *model.Config) {
			*cfg.ServiceSettings.ImageProxyType = ""
			*cfg.ServiceSettings.ImageProxyURL = ""
			*cfg.ServiceSettings.ImageProxyOptions = ""
		})

		return th
	}

	t.Run("no metadata needed", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		post := th.CreatePost(th.BasicChannel)
		message := post.Message

		clientPost, err := th.App.PreparePostForClient(post)
		require.Nil(t, err)

		assert.NotEqual(t, clientPost, post, "should've returned a new post")
		assert.Equal(t, message, post.Message, "shouldn't have mutated post.Message")
		assert.NotEqual(t, nil, post.ReactionCounts, "shouldn't have mutated post.ReactionCounts")
		assert.NotEqual(t, nil, post.FileInfos, "shouldn't have mutated post.FileInfos")
		assert.NotEqual(t, nil, post.Emojis, "shouldn't have mutated post.Emojis")
		assert.NotEqual(t, nil, post.ImageDimensions, "shouldn't have mutated post.ImageDimensions")
		assert.NotEqual(t, nil, post.OpenGraphData, "shouldn't have mutated post.OpenGraphData")

		assert.Equal(t, clientPost.Message, post.Message, "shouldn't have changed Message")
		assert.Len(t, post.ReactionCounts, 0, "should've populated ReactionCounts")
		assert.Len(t, post.FileInfos, 0, "should've populated FileInfos")
		assert.Len(t, post.Emojis, 0, "should've populated Emojis")
		assert.Len(t, post.ImageDimensions, 0, "should've populated ImageDimensions")
		assert.Len(t, post.OpenGraphData, 0, "should've populated OpenGraphData")
	})

	t.Run("metadata already set", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		post, err := th.App.PreparePostForClient(th.CreatePost(th.BasicChannel))
		require.Nil(t, err)

		clientPost, err := th.App.PreparePostForClient(post)
		require.Nil(t, err)

		assert.False(t, clientPost == post, "should've returned a new post")
		assert.Equal(t, clientPost, post, "shouldn't have changed any metadata")
	})

	t.Run("reaction counts", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		post := th.CreatePost(th.BasicChannel)
		th.AddReactionToPost(post, th.BasicUser, "smile")

		clientPost, err := th.App.PreparePostForClient(post)
		require.Nil(t, err)

		assert.Equal(t, model.ReactionCounts{
			"smile": 1,
		}, clientPost.ReactionCounts, "should've populated post.ReactionCounts")
	})

	t.Run("file infos", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		fileInfo, err := th.App.DoUploadFile(time.Now(), th.BasicTeam.Id, th.BasicChannel.Id, th.BasicUser.Id, "test.txt", []byte("test"))
		require.Nil(t, err)

		post, err := th.App.CreatePost(&model.Post{
			UserId:    th.BasicUser.Id,
			ChannelId: th.BasicChannel.Id,
			FileIds:   []string{fileInfo.Id},
		}, th.BasicChannel, false)
		require.Nil(t, err)

		fileInfo.PostId = post.Id

		clientPost, err := th.App.PreparePostForClient(post)
		require.Nil(t, err)

		assert.Equal(t, []*model.FileInfo{fileInfo}, clientPost.FileInfos, "should've populated post.FileInfos")
	})

	t.Run("emojis", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		emoji1 := th.CreateEmoji()
		emoji2 := th.CreateEmoji()
		emoji3 := th.CreateEmoji()

		post, err := th.App.CreatePost(&model.Post{
			UserId:    th.BasicUser.Id,
			ChannelId: th.BasicChannel.Id,
			Message:   ":" + emoji3.Name + ": :taco:",
		}, th.BasicChannel, false)
		require.Nil(t, err)

		th.AddReactionToPost(post, th.BasicUser, emoji1.Name)
		th.AddReactionToPost(post, th.BasicUser, emoji2.Name)
		th.AddReactionToPost(post, th.BasicUser2, emoji2.Name)

		clientPost, err := th.App.PreparePostForClient(post)
		require.Nil(t, err)

		assert.Equal(t, model.ReactionCounts{
			emoji1.Name: 1,
			emoji2.Name: 2,
		}, clientPost.ReactionCounts, "should've populated post.ReactionCounts")
		assert.ElementsMatch(t, []*model.Emoji{emoji1, emoji2, emoji3}, clientPost.Emojis, "should've populated post.Emojis")
	})

	t.Run("proxy linked images", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		testProxyLinkedImage(t, th, false)
	})

	t.Run("proxy opengraph images", func(t *testing.T) {
		// TODO
	})
}

func TestPreparePostForClientWithImageProxy(t *testing.T) {
	setup := func() *TestHelper {
		th := Setup().InitBasic()

		th.App.UpdateConfig(func(cfg *model.Config) {
			*cfg.ServiceSettings.SiteURL = "http://mymattermost.com"
			*cfg.ServiceSettings.ImageProxyType = "atmos/camo"
			*cfg.ServiceSettings.ImageProxyURL = "https://127.0.0.1"
			*cfg.ServiceSettings.ImageProxyOptions = "foo"
		})

		return th
	}

	t.Run("proxy linked images", func(t *testing.T) {
		th := setup()
		defer th.TearDown()

		testProxyLinkedImage(t, th, true)
	})

	t.Run("proxy opengraph images", func(t *testing.T) {
		// TODO
	})
}

func testProxyLinkedImage(t *testing.T, th *TestHelper, shouldProxy bool) {
	postTemplate := "![foo](%v)"
	imageURL := "http://mydomain.com/myimage"
	proxiedImageURL := "https://127.0.0.1/f8dace906d23689e8d5b12c3cefbedbf7b9b72f5/687474703a2f2f6d79646f6d61696e2e636f6d2f6d79696d616765"

	post := &model.Post{
		UserId:    th.BasicUser.Id,
		ChannelId: th.BasicChannel.Id,
		Message:   fmt.Sprintf(postTemplate, imageURL),
	}

	var err *model.AppError
	post, err = th.App.CreatePost(post, th.BasicChannel, false)
	require.Nil(t, err)

	clientPost, err := th.App.PreparePostForClient(post)
	require.Nil(t, err)

	if shouldProxy {
		assert.Equal(t, post.Message, fmt.Sprintf(postTemplate, imageURL), "should not have mutated original post")
		assert.Equal(t, clientPost.Message, fmt.Sprintf(postTemplate, proxiedImageURL), "should've replaced linked image URLs")
	} else {
		assert.Equal(t, clientPost.Message, fmt.Sprintf(postTemplate, imageURL), "shouldn't have replaced linked image URLs")
	}
}

func TestImageProxy(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	th.App.UpdateConfig(func(cfg *model.Config) {
		*cfg.ServiceSettings.SiteURL = "http://mymattermost.com"
	})

	for name, tc := range map[string]struct {
		ProxyType       string
		ProxyURL        string
		ProxyOptions    string
		ImageURL        string
		ProxiedImageURL string
	}{
		"atmos/camo": {
			ProxyType:       "atmos/camo",
			ProxyURL:        "https://127.0.0.1",
			ProxyOptions:    "foo",
			ImageURL:        "http://mydomain.com/myimage",
			ProxiedImageURL: "https://127.0.0.1/f8dace906d23689e8d5b12c3cefbedbf7b9b72f5/687474703a2f2f6d79646f6d61696e2e636f6d2f6d79696d616765",
		},
		"atmos/camo_SameSite": {
			ProxyType:       "atmos/camo",
			ProxyURL:        "https://127.0.0.1",
			ProxyOptions:    "foo",
			ImageURL:        "http://mymattermost.com/myimage",
			ProxiedImageURL: "http://mymattermost.com/myimage",
		},
		"atmos/camo_PathOnly": {
			ProxyType:       "atmos/camo",
			ProxyURL:        "https://127.0.0.1",
			ProxyOptions:    "foo",
			ImageURL:        "/myimage",
			ProxiedImageURL: "/myimage",
		},
		"atmos/camo_EmptyImageURL": {
			ProxyType:       "atmos/camo",
			ProxyURL:        "https://127.0.0.1",
			ProxyOptions:    "foo",
			ImageURL:        "",
			ProxiedImageURL: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			th.App.UpdateConfig(func(cfg *model.Config) {
				cfg.ServiceSettings.ImageProxyType = model.NewString(tc.ProxyType)
				cfg.ServiceSettings.ImageProxyOptions = model.NewString(tc.ProxyOptions)
				cfg.ServiceSettings.ImageProxyURL = model.NewString(tc.ProxyURL)
			})

			post := &model.Post{
				Id:      model.NewId(),
				Message: "![foo](" + tc.ImageURL + ")",
			}

			list := model.NewPostList()
			list.Posts[post.Id] = post

			assert.Equal(t, "![foo]("+tc.ProxiedImageURL+")", th.App.PostWithProxyAddedToImageURLs(post).Message)

			assert.Equal(t, "![foo]("+tc.ImageURL+")", th.App.PostWithProxyRemovedFromImageURLs(post).Message)
			post.Message = "![foo](" + tc.ProxiedImageURL + ")"
			assert.Equal(t, "![foo]("+tc.ImageURL+")", th.App.PostWithProxyRemovedFromImageURLs(post).Message)
		})
	}
}

func BenchmarkForceHTMLEncodingToUTF8(b *testing.B) {
	HTML := `
		<html>
			<head>
				<meta property="og:url" content="https://example.com/apps/mattermost">
				<meta property="og:image" content="https://images.example.com/image.png">
			</head>
		</html>
	`
	ContentType := "text/html; utf-8"

	b.Run("with converting", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			r := forceHTMLEncodingToUTF8(strings.NewReader(HTML), ContentType)

			og := opengraph.NewOpenGraph()
			og.ProcessHTML(r)
		}
	})

	b.Run("without converting", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			og := opengraph.NewOpenGraph()
			og.ProcessHTML(strings.NewReader(HTML))
		}
	})
}

func TestMakeOpenGraphURLsAbsolute(t *testing.T) {
	for name, tc := range map[string]struct {
		HTML       string
		RequestURL string
		URL        string
		ImageURL   string
	}{
		"absolute URLs": {
			HTML: `
				<html>
					<head>
						<meta property="og:url" content="https://example.com/apps/mattermost">
						<meta property="og:image" content="https://images.example.com/image.png">
					</head>
				</html>`,
			RequestURL: "https://example.com",
			URL:        "https://example.com/apps/mattermost",
			ImageURL:   "https://images.example.com/image.png",
		},
		"URLs starting with /": {
			HTML: `
				<html>
					<head>
						<meta property="og:url" content="/apps/mattermost">
						<meta property="og:image" content="/image.png">
					</head>
				</html>`,
			RequestURL: "http://example.com",
			URL:        "http://example.com/apps/mattermost",
			ImageURL:   "http://example.com/image.png",
		},
		"HTTPS URLs starting with /": {
			HTML: `
				<html>
					<head>
						<meta property="og:url" content="/apps/mattermost">
						<meta property="og:image" content="/image.png">
					</head>
				</html>`,
			RequestURL: "https://example.com",
			URL:        "https://example.com/apps/mattermost",
			ImageURL:   "https://example.com/image.png",
		},
		"missing image URL": {
			HTML: `
				<html>
					<head>
						<meta property="og:url" content="/apps/mattermost">
					</head>
				</html>`,
			RequestURL: "http://example.com",
			URL:        "http://example.com/apps/mattermost",
			ImageURL:   "",
		},
		"relative URLs": {
			HTML: `
				<html>
					<head>
						<meta property="og:url" content="index.html">
						<meta property="og:image" content="../resources/image.png">
					</head>
				</html>`,
			RequestURL: "http://example.com/content/index.html",
			URL:        "http://example.com/content/index.html",
			ImageURL:   "http://example.com/resources/image.png",
		},
	} {
		t.Run(name, func(t *testing.T) {
			og := opengraph.NewOpenGraph()
			if err := og.ProcessHTML(strings.NewReader(tc.HTML)); err != nil {
				t.Fatal(err)
			}

			makeOpenGraphURLsAbsolute(og, tc.RequestURL)

			if og.URL != tc.URL {
				t.Fatalf("incorrect url, expected %v, got %v", tc.URL, og.URL)
			}

			if len(og.Images) > 0 {
				if og.Images[0].URL != tc.ImageURL {
					t.Fatalf("incorrect image url, expected %v, got %v", tc.ImageURL, og.Images[0].URL)
				}
			} else if tc.ImageURL != "" {
				t.Fatalf("missing image url, expected %v, got nothing", tc.ImageURL)
			}
		})
	}
}

func TestMaxPostSize(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		Description         string
		StoreMaxPostSize    int
		ExpectedMaxPostSize int
		ExpectedError       *model.AppError
	}{
		{
			"error fetching max post size",
			0,
			model.POST_MESSAGE_MAX_RUNES_V1,
			model.NewAppError("TestMaxPostSize", "this is an error", nil, "", http.StatusBadRequest),
		},
		{
			"4000 rune limit",
			4000,
			4000,
			nil,
		},
		{
			"16383 rune limit",
			16383,
			16383,
			nil,
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.Description, func(t *testing.T) {
			t.Parallel()

			mockStore := &storetest.Store{}
			defer mockStore.AssertExpectations(t)

			mockStore.PostStore.On("GetMaxPostSize").Return(
				storetest.NewStoreChannel(store.StoreResult{
					Data: testCase.StoreMaxPostSize,
					Err:  testCase.ExpectedError,
				}),
			)

			app := App{
				Srv: &Server{
					Store: mockStore,
				},
				config: atomic.Value{},
			}

			assert.Equal(t, testCase.ExpectedMaxPostSize, app.MaxPostSize())
		})
	}
}

func TestGetCustomEmojisForPost_Message(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	emoji1 := th.CreateEmoji()
	emoji2 := th.CreateEmoji()
	emoji3 := th.CreateEmoji()

	testCases := []struct {
		Description      string
		Input            string
		Expected         []*model.Emoji
		SkipExpectations bool
	}{
		{
			Description:      "no emojis",
			Input:            "this is a string",
			Expected:         []*model.Emoji{},
			SkipExpectations: true,
		},
		{
			Description: "one emoji",
			Input:       "this is an :" + emoji1.Name + ": string",
			Expected: []*model.Emoji{
				emoji1,
			},
		},
		{
			Description: "two emojis",
			Input:       "this is a :" + emoji3.Name + ": :" + emoji2.Name + ": string",
			Expected: []*model.Emoji{
				emoji3,
				emoji2,
			},
		},
		{
			Description: "punctuation around emojis",
			Input:       ":" + emoji3.Name + ":/:" + emoji1.Name + ": (:" + emoji2.Name + ":)",
			Expected: []*model.Emoji{
				emoji3,
				emoji1,
				emoji2,
			},
		},
		{
			Description: "adjacent emojis",
			Input:       ":" + emoji3.Name + "::" + emoji1.Name + ":",
			Expected: []*model.Emoji{
				emoji3,
				emoji1,
			},
		},
		{
			Description: "duplicate emojis",
			Input:       "" + emoji1.Name + ": :" + emoji1.Name + ": :" + emoji1.Name + ": :" + emoji2.Name + ": :" + emoji2.Name + ": :" + emoji1.Name + ":",
			Expected: []*model.Emoji{
				emoji1,
				emoji2,
			},
		},
		{
			Description: "fake emojis",
			Input:       "these don't exist :tomato: :potato: :rotato:",
			Expected:    []*model.Emoji{},
		},
		{
			Description: "fake and real emojis",
			Input:       ":tomato::" + emoji1.Name + ": :potato: :" + emoji2.Name + ":",
			Expected: []*model.Emoji{
				emoji1,
				emoji2,
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.Description, func(t *testing.T) {
			emojis, err := th.App.getCustomEmojisForPost(testCase.Input, nil)
			assert.Nil(t, err, "failed to get emojis in message")
			assert.ElementsMatch(t, emojis, testCase.Expected, "received incorrect emojis")
		})
	}
}

func TestGetCustomEmojisForPost(t *testing.T) {
	th := Setup().InitBasic()
	defer th.TearDown()

	emoji1 := th.CreateEmoji()
	emoji2 := th.CreateEmoji()

	reactions := []*model.Reaction{
		{
			UserId:    th.BasicUser.Id,
			EmojiName: emoji1.Name,
		},
	}

	emojis, err := th.App.getCustomEmojisForPost(":"+emoji2.Name+":", reactions)
	assert.Nil(t, err, "failed to get emojis for post")
	assert.ElementsMatch(t, emojis, []*model.Emoji{emoji1, emoji2}, "received incorrect emojis")
}
